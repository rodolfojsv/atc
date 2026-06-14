// Package ntfy turns atc session lifecycle events into push notifications
// via an ntfy server (https://ntfy.sh or self-hosted). It is an outbound-only
// bus subscriber: on waiting-on-permission / finished / error it POSTs a
// message to the topic of whoever started the session, so a blocked or
// finished agent surfaces on that person's phone — even backgrounded, which
// the in-page service-worker notifications can't do.
//
// Routing is per-device: each web/app client registers a secret topic that
// rides along on the sessions it creates (Session.NotifyTopic). The publisher
// resolves a session's topic at send time via the topicOf callback, falling
// back to a single configured Topic for sessions without one (TUI/scheduler).
package ntfy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rodolfojsv/atc/internal/bus"
	"github.com/rodolfojsv/atc/internal/config"
)

// Publisher posts atc events to an ntfy server. The zero value is unusable;
// build one with New.
type Publisher struct {
	server     string // base URL atc POSTs to, no trailing slash
	defTopic   string
	token      string // ntfy access token (optional)
	serverName string // label in the title
	publicURL  string // atc's own URL, for deep links / action buttons
	actions    bool
	atcToken   string // atc bearer token, for Approve/Deny action buttons
	client     *http.Client
	topicOf    func(sessionName string) string // "" if the session has no topic
}

// New builds a Publisher from config. atcToken is atc's own bearer token
// (used only for Approve/Deny action buttons, and only when cfg.Actions is
// set). topicOf maps a session name to its per-device notify topic.
func New(cfg config.Ntfy, atcToken string, topicOf func(string) string) *Publisher {
	server := strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	if server == "" {
		server = "https://ntfy.sh"
	}
	name := strings.TrimSpace(cfg.ServerName)
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		} else {
			name = "atc"
		}
	}
	if topicOf == nil {
		topicOf = func(string) string { return "" }
	}
	return &Publisher{
		server:     server,
		defTopic:   strings.TrimSpace(cfg.Topic),
		token:      strings.TrimSpace(cfg.Token),
		serverName: name,
		publicURL:  strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
		actions:    cfg.Actions,
		atcToken:   atcToken,
		client:     &http.Client{Timeout: 10 * time.Second},
		topicOf:    topicOf,
	}
}

// OnEvent is the bus subscriber. It returns immediately; the HTTP POST runs
// on its own goroutine so a slow ntfy server never stalls other subscribers.
func (p *Publisher) OnEvent(e bus.Event) {
	title, tag, prio, ok := classify(e)
	if !ok {
		return
	}
	for _, topic := range p.recipients(e.SessionName) {
		go p.post(topic, title, body(e), prio, tag, e)
	}
}

// recipients is the de-duplicated set of topics to notify for a session: the
// creator's per-device topic (if the session carries one) AND the configured
// default topic (if set). The default makes a single subscribed phone receive
// alerts for every session — including TUI/scheduler ones and any created
// before per-device topics existed — which is what a one-person setup wants.
func (p *Publisher) recipients(sessionName string) []string {
	pd := p.topicOf(sessionName)
	if pd == MuteSentinel {
		return nil // user unchecked "notify" at creation — silence it entirely
	}
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	add(pd)
	add(p.defTopic)
	return out
}

// MuteSentinel is the notifyTopic value a client sets to suppress all push
// for a session (overrides even the default topic).
const MuteSentinel = "-"

// classify maps an event to a title suffix, tag, and priority, and reports
// whether it's a notify-worthy event at all.
func classify(e bus.Event) (title, tag string, prio int, ok bool) {
	switch e.Type {
	case bus.WaitingOnPermission:
		return "needs approval", "warning", 4, true
	case bus.Finished:
		return "finished", "white_check_mark", 3, true
	case bus.Error:
		return "errored", "rotating_light", 4, true
	}
	return "", "", 0, false
}

func body(e bus.Event) string {
	for _, k := range []string{"summary", "lastLine", "message"} {
		if v, ok := e.Data[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

type ntfyAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title,omitempty"`
	Message  string       `json:"message,omitempty"`
	Priority int          `json:"priority,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Click    string       `json:"click,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

// build assembles the JSON message for an event (exported-ish for testing
// via Message).
func (p *Publisher) build(topic, title, msg string, prio int, tag string, e bus.Event) ntfyMessage {
	m := ntfyMessage{
		Topic:    topic,
		Title:    p.serverName + " · " + e.SessionName + " " + title,
		Message:  msg,
		Priority: prio,
	}
	if tag != "" {
		m.Tags = []string{tag}
	}
	if p.publicURL != "" {
		// Tokenless deep link: opened in a context that already holds the
		// token (the saved app/browser), it lands on the session; elsewhere
		// it lands on login. Either way no secret leaks into the message.
		m.Click = p.publicURL + "/?focus=" + url.QueryEscape(e.SessionName)
		if p.actions && e.Type == bus.WaitingOnPermission {
			base := p.publicURL + "/api/sessions/" + url.PathEscape(e.SessionName) + "/respond"
			h := map[string]string{"Authorization": "Bearer " + p.atcToken, "Content-Type": "application/json"}
			m.Actions = []ntfyAction{
				{Action: "http", Label: "Approve", URL: base, Method: "POST", Headers: h, Body: `{"decision":"approve"}`, Clear: true},
				{Action: "http", Label: "Deny", URL: base, Method: "POST", Headers: h, Body: `{"decision":"deny"}`, Clear: true},
			}
		}
	}
	return m
}

func (p *Publisher) post(topic, title, msg string, prio int, tag string, e bus.Event) {
	data, err := json.Marshal(p.build(topic, title, msg, prio, tag, e))
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, p.server+"/", bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
