package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/maltegrosse/go-modemmanager"
	"github.com/psanford/gsm/mms"
	"github.com/psanford/gsm/wap"
)

var postURL = flag.String("url", "", "URL to post messages to")
var username = flag.String("username", "", "Username")
var password = flag.String("password", "", "Username")
var slackToken = flag.String("slack-token", "", "Slack bot token")
var slackChannelID = flag.String("slack-channel", "", "Slack channel id")

func main() {
	flag.Parse()

	s := &server{
		inboundMsg:  make(chan PostMsg, 100),
		outboundMsg: make(chan OutboundMsg),
		slackLogMsg: make(chan string, 100),
	}
	s.initCMDS()
	go s.runSlack()
	s.run()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	<-sigs
}

type server struct {
	cmds        []cmd
	inboundMsg  chan PostMsg
	slackLogMsg chan string
	outboundMsg chan OutboundMsg
}

func (s *server) run() {
	mmgr, err := modemmanager.NewModemManager()
	if err != nil {
		log.Fatal(err.Error())
	}
	modems, err := mmgr.GetModems()
	if err != nil {
		log.Fatalf("Get modems err: %s", err)
	}
	if len(modems) < 1 {
		log.Fatalf("No modems detected: %s", err)
	}
	for _, modem := range modems {
		go s.listenSMS(modem)
	}
}

func (s *server) listenSMS(modem modemmanager.Modem) {
	mm, err := modem.GetMessaging()
	if err != nil {
		log.Fatal(err)
	}
	smses, err := mm.List()
	if err != nil {
		log.Fatal(err)
	}

	for _, sms := range smses {
		num, _ := sms.GetNumber()
		txt, _ := sms.GetText()
		ts, _ := sms.GetTimestamp()
		j, _ := sms.MarshalJSON()
		pdu, _ := sms.GetPduType()
		log.Println("init raw msg", string(j))
		if pdu != modemmanager.MmSmsPduTypeSubmit {
			log.Println("msg", string(j))
			log.Println("========================================")
			msg := PostMsg{
				From: num,
				TS:   ts,
				Body: txt,
			}
			s.forwardMsg(msg)
		}
		mm.Delete(sms)
	}

	ownNumbers, _ := modem.GetOwnNumbers()
	log.Printf("own numbers: %+v\n", ownNumbers)

	ticker := time.NewTicker(5 * time.Minute)
	pendingHealthChecks := make(map[string]time.Time)

	pingPath, err := exec.LookPath("ping")
	if err != nil {
		log.Printf("failed to find ping in path, disabling health check")
	}

	logDir := s.logDir()
	if logDir != "" {
		os.MkdirAll(logDir, 0700)
	}

	c := mm.SubscribeAdded()
	for {
		log.Printf("subscribe loop")
		select {
		case <-ticker.C:
			if pingPath != "" {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					err = exec.CommandContext(ctx, pingPath, "-I", "wwp0s19u1u3i12", "-c", "2", "8.8.8.8").Run()
					if err != nil {
						log.Printf("health check ping err: %s", err)
					}
				}()
			}

		case msg := <-s.outboundMsg:
			log.Printf("outbound msg")
			reply, err := mm.CreateSms(msg.To, msg.Body)
			if err != nil {
				log.Printf("create sms err: %s", err)
				msg.Result <- err
				continue
			}
			log.Printf("send msg to=%s body=%q", msg.To, msg.Body)
			err = reply.Send()
			if err != nil {
				log.Printf("reply send err: %s", err)
				msg.Result <- err
				continue
			}
			mm.Delete(reply)
			msg.Result <- nil
		case added := <-c:
			log.Printf("got inbound sms: %+v\n", added)
			sms, recieved, err := mm.ParseAdded(added)
			if err != nil {
				log.Fatalf("failed to parse sms: %s", err)
			}
			j, err := sms.MarshalJSON()
			if err != nil {
				log.Printf("sms marshal json err: %s", err)
			} else {
				if logDir != "" {
					ts := time.Now().UnixMilli()
					p := filepath.Join(logDir, fmt.Sprintf("sms.%d", ts))
					os.WriteFile(p, j, 0600)
				}
			}
			if !recieved {
				log.Printf("outbound msg (skip without delete): %+v %s", sms, string(j))
				continue
			}
			num, _ := sms.GetNumber()
			txt, _ := sms.GetText()
			ts, _ := sms.GetTimestamp()
			data, _ := sms.GetData()

			var attachments []Attachment
			var tos []string

			if len(data) > 0 {
				packet, err := wap.UnmarshalPushNotification(data)
				if err != nil {
					log.Printf("decode wap message err: %s", err)
				}

				log.Printf("parsed WAP-Push msg: %+v", packet)
				if from := packet.Header.Get(mms.From.String()); from != "" {
					num = from
				}
				for _, to := range packet.Header[mms.To.String()] {
					tos = append(tos, to)
				}

				contentLocation := packet.Header.Get(mms.ContentLocation.String())
				if contentLocation != "" {
					msg, rawBody, err := s.fetchMMS(contentLocation)
					if rawBody != nil {
						if logDir != "" {
							ts := time.Now().UnixMilli()
							safeLoc := strings.ReplaceAll(contentLocation, "/", "_")
							p := filepath.Join(logDir, fmt.Sprintf("mms.%d.%s", ts, filepath.Clean(safeLoc)))
							log.Printf("log to %s", p)
							os.WriteFile(p, rawBody, 0600)
						}
					}
					if err != nil {
						log.Printf("fetch mms err: %s", err)
					} else {
						log.Printf("mms msg header: %+v", msg.Header)
						for _, part := range msg.Parts {
							if part.ContentType == "text/plain" {
								txt = txt + string(part.Data)
							} else {
								name := part.FileName
								if name == "" {
									name = part.Header["Name"]
								}

								attachments = append(attachments, Attachment{
									ContentType: part.ContentType,
									Name:        name,
									Data:        part.Data,
								})
							}
						}
					}
				}
			}

			log.Println("msg", string(j))
			log.Println("========================================")
			mm.Delete(sms)

			if _, found := pendingHealthChecks[txt]; found {
				delete(pendingHealthChecks, txt)
				log.Printf("Health check ok")
				continue
			}

			msg := PostMsg{
				From:        num,
				TS:          ts,
				To:          strings.Join(tos, ", "),
				Tos:         tos,
				Body:        txt,
				Attachments: attachments,
			}
			s.forwardMsg(msg)
		}
	}
}

func (s *server) forwardMsg(msg PostMsg) error {
	select {
	case s.inboundMsg <- msg:
	default:
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("Marshal msg err: %s", err)
	}

	req, err := http.NewRequest("POST", *postURL, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("NewRequest err: %s", err)
	}

	req.SetBasicAuth(*username, *password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Post error: %s", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("Post error: %d %+v", resp.StatusCode, resp)
	}

	return nil
}

func (s *server) fetchMMS(url string) (*mms.Message, []byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch MMS %s err: %w", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch MMS read body %s err: %w", url, err)
	}

	msg, err := mms.Unmarshal(body)
	return msg, body, err
}

func (s *server) logMsgToSlack(msg string) {
	select {
	case s.slackLogMsg <- msg:
	default:
	}
}

func (s *server) SendSMS(to string, body string) error {
	msg := OutboundMsg{
		To:     to,
		Body:   body,
		Result: make(chan error, 1),
	}
	s.outboundMsg <- msg
	err := <-msg.Result
	return err
}

func (s *server) logDir() string {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		return ""
	}
	return filepath.Join(homeDir, ".cache/sillio")
}

type PostMsg struct {
	From        string       `json:"from"`
	To          string       `json:"to"`
	Tos         []string     `json:"tos"`
	TS          time.Time    `json:"ts"`
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachment"`
}

type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

type OutboundMsg struct {
	To     string
	Body   string
	Result chan error
}
