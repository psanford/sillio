package main

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nyaruka/phonenumbers"
	"github.com/retailnext/unixtime"
	"github.com/slack-go/slack"
)

func (s *server) helpMessage() string {
	var b strings.Builder
	b.WriteString("Usage:\n")
	for _, c := range s.cmds {
		if c.helpText != "" {
			b.WriteString(c.helpText)
		} else {
			b.WriteString(c.name)
			for _, arg := range c.args {
				b.WriteString(" ")
				b.WriteString(arg)
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (s *server) helpCMD(c cmdInfo) {
	c.respond(s.helpMessage())
}

func (s *server) smsCMD(c cmdInfo) {
	if len(c.args) < 2 {
		c.respond(s.helpMessage())
		return
	}

	phoneNumber := c.args[0]
	msg := strings.Join(c.args[1:], " ")

	parsed, err := phonenumbers.Parse(phoneNumber, "US")
	if err != nil {
		log.Printf("parse number err: %s", err)
		c.respond(fmt.Sprintf("parse number err: %s", err))
		return
	}

	num := phonenumbers.Format(parsed, phonenumbers.E164)
	num = strings.TrimPrefix(num, "+")

	err = s.SendSMS(num, msg)
	if err != nil {
		log.Printf("Send sms err: %s", err)
		c.respond(fmt.Sprintf("Send sms err: %s", err))
		return
	}

	c.respond("Send ok!")
}

func (s *server) initCMDS() {
	s.cmds = []cmd{
		{
			name:   "help",
			action: s.helpCMD,
		},
		{
			name:     "sms",
			splat:    true,
			helpText: "sms <phone_number> <message>",
			action:   s.smsCMD,
		},
	}

	sort.Slice(s.cmds, func(a, b int) bool {
		return s.cmds[a].name < s.cmds[b].name
	})
}

type cmdInfo struct {
	rawText string
	args    []string
	rtm     *slack.RTM
	respond func(string)
}

type cmd struct {
	name     string
	args     []string
	splat    bool
	helpText string
	action   func(c cmdInfo)
}

func (s *server) runSlack() {
	api := slack.New(*slackToken)
	rtm := api.NewRTM()
	go rtm.ManageConnection()

	var info *slack.Info

	for {
		select {
		case msg := <-s.slackLogMsg:
			rtm.SendMessage(rtm.NewOutgoingMessage(msg, *slackChannelID))
		case smsMsg := <-s.inboundMsg:
			_, _, err := api.PostMessage(*slackChannelID, slack.MsgOptionAttachments(slack.Attachment{
				Title: fmt.Sprintf("From %s", smsMsg.From),
				Text:  smsMsg.Body,
				Fields: []slack.AttachmentField{
					{
						Title: "Time",
						Value: smsMsg.TS.Format(time.RFC3339),
						Short: true,
					},
					{
						Title: "To",
						Value: smsMsg.To,
						Short: true,
					},
				},
			}))
			if err != nil {
				log.Printf("post message err: %s", err)
			}

			for _, part := range smsMsg.Attachments {
				name := part.Name
				if name == "" {
					name = "_blank_"
				}
				_, err := api.UploadFile(slack.FileUploadParameters{
					Filename: part.Name,
					Content:  string(part.Data),
					Channels: []string{*slackChannelID},
				})
				if err != nil {
					log.Printf("upload file %s err: %s", part.Name, err)
				}
			}

		case msg := <-rtm.IncomingEvents:
			switch ev := msg.Data.(type) {
			case *slack.HelloEvent:
				info = rtm.GetInfo()
				log.Printf("slack info: %+v\n", info)

			case *slack.MessageEvent:
				log.Printf("got_msg msg=%+v ev=%+v user=%+v", msg, ev, ev.User)
				if ev.User == info.User.ID {
					// ignore messages from myself
					continue
				}
				msg := strings.TrimSpace(strings.ToLower(ev.Text))

				tsFloat, err := strconv.ParseFloat(ev.EventTimestamp, 64)
				if err != nil {
					log.Printf("parse eventime error")
				}

				ts := unixtime.ToTime(int64(tsFloat*1000.0), time.Millisecond)

				if time.Since(ts) > time.Hour {
					log.Printf("ignoring slack msg older than an hour ago: %s", ts)
					continue
				}

				msgFields := strings.Fields(msg)
				var firstField string
				var args []string
				if len(msgFields) > 0 {
					firstField = msgFields[0]
					args = msgFields[1:]
				}

				var cInfo = cmdInfo{
					rawText: ev.Text,
					args:    args,
					rtm:     rtm,
					respond: func(s string) {
						rtm.SendMessage(rtm.NewOutgoingMessage(s, ev.Channel))
					},
				}

				if ev.Channel != *slackChannelID {
					log.Printf("Wrong channel")
					continue
				}

				for _, c := range s.cmds {
					if firstField == c.name {
						if c.splat || len(c.args) == len(args) {
							c.action(cInfo)
						} else {
							rtm.SendMessage(rtm.NewOutgoingMessage(s.helpMessage(), ev.Channel))
						}
						break
					}
				}

			case *slack.InvalidAuthEvent:
				log.Printf("Invalid credentials slack")
				return
			default:
				// Ignore other events..
				// log.Printf("ignored_event type=%s data=%+v", msg.Type, msg.Data)
			}
		}
	}

}
