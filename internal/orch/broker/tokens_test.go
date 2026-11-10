package broker

import (
	"context"
	"net"
	"strings"
	"testing"
)

// The client decodes `tokens` rows leniently: a newer broker may add fields
// (today `usable`, tomorrow more) and that must not break the list.
func TestTokensRowsTolerateExtraFields(t *testing.T) {
	sock := serve(t, func(c net.Conn, line string) {
		if !strings.Contains(line, `"op":"tokens"`) {
			c.Write([]byte("{\"ok\":false,\"code\":\"unknown_op\"}\n"))
			return
		}
		c.Write([]byte(`{"ok":true,"result":{"tokens":[` +
			`{"ref":"openai","kind":"openai","last4":"","expires":"","set_at":"","source":"config","days_left":null,"usable":true,"future":{"x":[1,2]},"another":"y"},` +
			`{"ref":"gitlab-main","kind":"gitlab","last4":"ZQxw","expires":"2099-12-31","set_at":"2026-11-10T01:02:03Z","source":"store","days_left":12,"usable":false,"unknown_flag":true}` +
			`],"warn_days":14,"expiring":[{"ref":"x"}],"future_top":1}}` + "\n"))
	})
	list, oerr := (&Client{Socket: sock}).Tokens(context.Background())
	if oerr != nil {
		t.Fatalf("%v", oerr)
	}
	if len(list.Tokens) != 2 || list.WarnDays != 14 {
		t.Fatalf("%+v", list)
	}
	a, b := list.Tokens[0], list.Tokens[1]
	if a.Source != "config" || !a.Usable || a.DaysLeft != nil || a.Ref != "openai" {
		t.Errorf("row0 %+v", a)
	}
	if b.Source != "store" || b.Usable || b.DaysLeft == nil || *b.DaysLeft != 12 || b.Last4 != "ZQxw" {
		t.Errorf("row1 %+v", b)
	}
}
