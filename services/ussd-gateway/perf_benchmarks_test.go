package main

import "testing"

func BenchmarkRender(b *testing.B) {
	sess := &Session{Data: map[string]string{"name": "Aminu", "tin": "12345678-0001"}}
	for i := 0; i < b.N; i++ {
		_ = render("Hello {{name}}, TIN {{tin}}. {{missing}}", sess)
	}
}

func BenchmarkValidateInput(b *testing.B) {
	g, err := LoadMenuGraph()
	if err != nil {
		b.Fatal(err)
	}
	var menuID string
	for id, m := range g.Menus {
		if m.Type == "input" && m.Validate != "" {
			menuID = id
			break
		}
	}
	if menuID == "" {
		b.Skip("no validating input menu")
	}
	eng := NewEngine(g, map[string]ActionHandler{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sess := &Session{Menu: menuID, Data: map[string]string{}}
		_, _, _ = eng.Handle(sess, "12345")
	}
}
