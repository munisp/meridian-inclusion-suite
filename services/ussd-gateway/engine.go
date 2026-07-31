package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed menus.json
var menusFS embed.FS

// MenuGraph is the JSON menu DSL document.
type MenuGraph struct {
	ServiceCode       string          `json:"service_code"`
	Start             string          `json:"start"`
	SessionTTLSeconds int             `json:"session_ttl_seconds"`
	Menus             map[string]Menu `json:"menus"`
}

// Menu is one DSL node. Types:
//   - options: render text; user key selects an option (may set vars) -> next
//   - input:   render text; validate regex; save input as save_as -> next
//   - action:  run a registered action handler -> next | error_next
//   - end:     render text and terminate the session
type Menu struct {
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Options     []MenuOption `json:"options,omitempty"`
	SaveAs      string       `json:"save_as,omitempty"`
	Validate    string       `json:"validate,omitempty"`
	InvalidText string       `json:"invalid_text,omitempty"`
	Action      string       `json:"action,omitempty"`
	Next        string       `json:"next,omitempty"`
	ErrorNext   string       `json:"error_next,omitempty"`
}

// MenuOption is one numbered choice.
type MenuOption struct {
	Key  string            `json:"key"`
	Next string            `json:"next"`
	Set  map[string]string `json:"set,omitempty"`
}

// LoadMenuGraph loads the embedded menus.json DSL.
func LoadMenuGraph() (*MenuGraph, error) {
	b, err := menusFS.ReadFile("menus.json")
	if err != nil {
		return nil, err
	}
	var g MenuGraph
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("menus.json: %w", err)
	}
	if g.Start == "" || g.Menus[g.Start].Type == "" {
		return nil, fmt.Errorf("menus.json: start menu %q missing", g.Start)
	}
	if g.SessionTTLSeconds == 0 {
		g.SessionTTLSeconds = 180
	}
	return &g, nil
}

// ActionHandler executes a DSL action node against the session.
type ActionHandler func(sess *Session) error

// Engine interprets the menu graph.
type Engine struct {
	graph   *MenuGraph
	actions map[string]ActionHandler
}

func NewEngine(graph *MenuGraph, actions map[string]ActionHandler) *Engine {
	return &Engine{graph: graph, actions: actions}
}

// render expands {{var}} templates from session data.
func render(text string, sess *Session) string {
	out := text
	for k, v := range sess.Data {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	// unresolved placeholders become "-"
	out = regexp.MustCompile(`\{\{[a-z_]+\}\}`).ReplaceAllString(out, "-")
	return out
}

// Start returns the first screen for a new session.
func (e *Engine) Start(sess *Session) (string, bool, error) {
	sess.Menu = e.graph.Start
	return e.renderCurrent(sess, "")
}

// Handle processes one user input for the current menu and returns the next
// screen. The bool is true when the session should continue (CON) or false
// when terminated (END).
func (e *Engine) Handle(sess *Session, input string) (text string, cont bool, err error) {
	menu, ok := e.graph.Menus[sess.Menu]
	if !ok {
		return "Session error. Please redial.", false, fmt.Errorf("unknown menu %q", sess.Menu)
	}
	switch menu.Type {
	case "options":
		var chosen *MenuOption
		for i := range menu.Options {
			if menu.Options[i].Key == strings.TrimSpace(input) {
				chosen = &menu.Options[i]
				break
			}
		}
		if chosen == nil {
			return render(menu.Text, sess), true, nil // re-render same menu
		}
		for k, v := range chosen.Set {
			sess.Data[k] = v
		}
		sess.Menu = chosen.Next
	case "input":
		val := strings.TrimSpace(input)
		if menu.Validate != "" {
			re, rerr := regexp.Compile(menu.Validate)
			if rerr != nil {
				return "Configuration error.", false, rerr
			}
			if !re.MatchString(val) {
				msg := menu.InvalidText
				if msg == "" {
					msg = menu.Text
				}
				return render(msg, sess), true, nil
			}
		}
		if menu.SaveAs != "" {
			sess.Data[menu.SaveAs] = val
		}
		sess.Menu = menu.Next
	case "action":
		// actions execute without consuming input; chaining handled by caller
		return "", true, fmt.Errorf("action menus do not accept input")
	case "end":
		return render(menu.Text, sess), false, nil
	default:
		return "Session error.", false, fmt.Errorf("unknown menu type %q", menu.Type)
	}
	return e.renderCurrent(sess, input)
}

// renderCurrent renders the menu the session is now on, executing action
// nodes (and following their next links) until a displayable menu is reached.
func (e *Engine) renderCurrent(sess *Session, lastInput string) (string, bool, error) {
	for depth := 0; depth < 20; depth++ {
		menu, ok := e.graph.Menus[sess.Menu]
		if !ok {
			return "Session error. Please redial.", false, fmt.Errorf("unknown menu %q", sess.Menu)
		}
		switch menu.Type {
		case "action":
			handler, ok := e.actions[menu.Action]
			if !ok {
				return "Service unavailable.", false, fmt.Errorf("action %q not registered", menu.Action)
			}
			if err := handler(sess); err != nil {
				sess.Data["error"] = err.Error()
				if menu.ErrorNext != "" {
					sess.Menu = menu.ErrorNext
					continue
				}
				return "", false, err
			}
			// an action may override the success branch (e.g. onb.register
			// parking a registration as pending_review during an outage)
			if ov := sess.Data["_next_override"]; ov != "" {
				delete(sess.Data, "_next_override")
				sess.Menu = ov
			} else {
				sess.Menu = menu.Next
			}
		case "end":
			return render(localizedText(sess, menu), sess), false, nil
		default: // options | input
			return render(localizedText(sess, menu), sess), true, nil
		}
	}
	return "Session error.", false, fmt.Errorf("menu chain too deep")
}
