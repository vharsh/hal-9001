package pagerduty

/*
 * Copyright 2016-2017 Netflix, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import (
	"fmt"
	"strings"

	"github.com/netflix/hal-9001/hal"
)

const PageUsage = `!page <alias> [optional message]

Send an alert via Pagerduty with an optional custom message.

Aliases that have a comma-separated list of service keys will result in one page going to each service key when the alias is paged.

!page core
!page core <message>
!pagecore HELP ME YOU ARE MY ONLY HOPE

!page add <alias> <service key>
!page add <alias> <service key>,<service_key>,<service_key>,...
!page rm <alias>
!page list
`

const PageDefaultMessage = `your presence is requested in the chat room`

func page(msg hal.Evt) {
	parts := msg.BodyAsArgv()

	// detect concatenated command + team name & split them
	// e.g. "!pagecore" -> {"!page", "core"}
	if strings.HasPrefix(parts[0], "!page") && len(parts[0]) > 5 {
		team := strings.TrimPrefix(parts[0], "!page")
		parts = append([]string{"!page", team}, parts[1:]...)
	}

	// should be 2 parts now, "!page" and the target team at a minimum
	if parts[0] != "!page" || len(parts) < 2 {
		msg.Reply(PageUsage)
		return
	}

	switch parts[1] {
	case "h", "help":
		msg.Reply(PageUsage)
	case "add":
		addAlias(msg, parts[2:])
	case "rm":
		rmAlias(msg, parts[2:])
	case "list":
		listAlias(msg)
	default:
		pageAlias(msg, parts[1:])
	}
}

func pageAlias(evt hal.Evt, parts []string) {
	pageMessage := PageDefaultMessage
	msgPref := evt.AsPref().FindKey("default-message").Room(evt.RoomId).One()

	// Caller slices off the !page. parts[0] should be the alias.
	// Anything after is a custom message.
	if len(parts) > 1 {
		pageMessage = strings.Join(parts[1:], " ")
	} else if msgPref.Success {
		pageMessage = msgPref.Value
	}

	// map alias name to PD token via prefs
	key := aliasKey(parts[0])
	// make sure to filter on at least room id since FindKey might find duplicate
	// aliases from other rooms
	pref := evt.AsPref().FindKey(key).Room(evt.RoomId).One()

	// make sure the query succeeded
	if !pref.Success {
		if pref.Error != nil {
			evt.Replyf("Unable to access preferences: %#q", pref.Error)
		} else {
			evt.Replyf("Alias %q is not configured. Try !page add %s <pagerduty integration key>", parts[0], parts[0])
		}
		return
	}

	// if qpref.Get returned the default, the alias was not found
	if pref.Value == "" {
		evt.Replyf("Alias %q is not configured. Try !page add %s <pagerduty integration key>", parts[0], parts[0])
		return
	}

	// make sure the hal secrets are set up
	token, err := getSecrets()
	if err != nil {
		evt.Error(err)
		return
	}

	// the value can be a list of tokens, separated by commas
	for _, svckey := range strings.Split(pref.Value, ",") {
		// the v1 API silently swallows events sent with a v2 integration key
		// the v2 API *should* return a 202 when it can't immediately send the
		// event to a device, so we try v2 first and then v1 if that returns
		// anything other than a 200 OK.
		pde2 := NewV2Event()
		pde2.RoutingKey = svckey
		pde2.Action = "trigger"
		pde2.Payload.Summary = pageMessage
		pde2.Payload.Component = evt.BrokerName()
		pde2.Payload.Group = evt.Room
		pde2.Payload.Source = evt.User
		pde2.Payload.Class = "!page"
		resp2, err := pde2.Send(token)
		if err != nil {
			log.Printf("Pagerduty V2 API failed: %s", err)
			evt.Replyf("Pagerduty V2 API failed: %s", err)
			// do not bail out here - check the StatusCode and go on to try the V1 API if that check fails
		}

		// only a 200 is acceptable - a 202 means the service couldn't verify it actually sent the alert
		if resp2.StatusCode == 200 {
			log.Printf("Pagerduty V2 response message for %q -> %s(%s): %s\n", pageMessage, parts[0], svckey, resp2.Message)
			evt.Replyf("Message sent to %s using integration key %s via Pagerduty V2 API.", parts[0], svckey)
		} else {
			log.Printf("Pagerduty V2 API returned: %d %s\nTrying again with V1 API...", resp2.StatusCode, resp2.Message)
			evt.Replyf("Pagerduty V2 API returned: %d %s\nTrying again with V1 API...", resp2.StatusCode, resp2.Message)

			// create the event and send it
			pde1 := NewTrigger(svckey, pageMessage) // in ./pagerduty.go
			resp1, err := pde1.Send(token)
			if err != nil {
				evt.Replyf("Error while communicating with Pagerduty V1 API: %d %s", resp1.StatusCode, resp1.Message)
				continue
			}

			log.Printf("Pagerduty V1 response message for %q -> %s(%s): %s\n", pageMessage, parts[0], svckey, resp1.Message)
			evt.Replyf("Message sent to %q using integration key %s via Pagerduty V1 API.", parts[0], svckey)
		}
	}
}

func addAlias(msg hal.Evt, parts []string) {
	if len(parts) < 2 {
		msg.Replyf("!page add requires 2 arguments, e.g. !page add sysadmins XXXXXXX")
		return
	} else if len(parts) > 2 {
		keys := strings.Replace(strings.Join(parts[1:], ","), ",,", ",", len(parts)-2)
		parts = []string{parts[0], keys}
	}

	pref := msg.AsPref()
	pref.User = "" // filled in by AsPref and unwanted
	pref.Key = aliasKey(parts[0])
	pref.Value = parts[1]

	err := pref.Set()
	if err != nil {
		msg.Replyf("Write failed: %s", err)
	} else {
		msg.Replyf("Added alias: %q -> %q", parts[0], parts[1])
	}
}

func rmAlias(msg hal.Evt, parts []string) {
	if len(parts) != 1 {
		msg.Replyf("!page rm requires 1 argument, e.g. !page rm sysadmins")
		return
	}

	pref := msg.AsPref()
	pref.User = "" // filled in by AsPref and unwanted
	pref.Key = aliasKey(parts[0])
	pref.Delete()

	msg.Replyf("Removed alias %q", parts[0])
}

func listAlias(msg hal.Evt) {
	pref := msg.AsPref()
	pref.User = "" // filled in by AsPref and unwanted
	prefs := pref.GetPrefs()
	data := prefs.Table()
	msg.ReplyTable(data[0], data[1:])
}

func aliasKey(alias string) string {
	return fmt.Sprintf("alias.%s", alias)
}
