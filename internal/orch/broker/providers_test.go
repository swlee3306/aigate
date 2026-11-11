package broker

import (
	"context"
	"net"
	"testing"
	"time"
)

// create_partial is the only failure that carries a payload: the orphan
// facts (public values). The client validates every field and keeps nothing
// else; a malformed or hostile detail is dropped, never half-kept.
func TestCreatePartialDetailKept(t *testing.T) {
	good := `{"provider":"gl","project":"team/new-app","project_id":42,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false}`
	for name, tc := range map[string]struct {
		reply  string
		want   bool
		wantID int64
	}{
		"good":                {`{"ok":false,"code":"create_partial","message":"SECRET-TEXT","detail":` + good + `}`, true, 42},
		"null project id":     {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/new-app","project_id":null,"step":"project","orphan_recorded":false,"orphan_dropped":false}}`, true, 0},
		"no detail":           {`{"ok":false,"code":"create_partial"}`, false, 0},
		"detail on other":     {`{"ok":false,"code":"gitlab_refused","detail":` + good + `}`, false, 0},
		"bad provider":        {`{"ok":false,"code":"create_partial","detail":{"provider":"G L","project":"team/new-app","project_id":1,"step":"project","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
		"bad project":         {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"../x","project_id":1,"step":"project","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
		"bad step":            {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/x","project_id":1,"step":"rm -rf","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
		"unknown key":         {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/xx","project_id":1,"step":"project","orphan_recorded":true,"orphan_dropped":false,"token":"x"}}`, false, 0},
		"negative id":         {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/xx","project_id":-3,"step":"project","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
		"detail not object":   {`{"ok":false,"code":"create_partial","detail":"text"}`, false, 0},
		"id not a number":     {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/xx","project_id":"7","step":"project","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
		"recorded not bool":   {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/xx","project_id":1,"step":"project","orphan_recorded":"yes","orphan_dropped":false}}`, false, 0},
		"duplicate key":       {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","provider":"gl","project":"team/xx","project_id":1,"step":"project","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
		"six-segment project": {`{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"aa/bb/cc/dd/ee/ff","project_id":1,"step":"project","orphan_recorded":true,"orphan_dropped":false}}`, false, 0},
	} {
		sock := serve(t, func(c net.Conn, _ string) { c.Write([]byte(tc.reply + "\n")) })
		_, oerr := (&Client{Socket: sock}).Do(context.Background(), OpCreateProject, map[string]any{})
		if oerr == nil || (oerr.Code != "create_partial") != (name == "detail on other") {
			t.Fatalf("%s: %v", name, oerr)
		}
		d := oerr.Partial
		if (d != nil) != tc.want {
			t.Errorf("%s: detail kept=%v want %v", name, d != nil, tc.want)
			continue
		}
		if d != nil {
			if d.Provider != "gl" || d.Project != "team/new-app" || d.Step == "" {
				t.Errorf("%s: %+v", name, d)
			}
			if (tc.wantID == 0) != (d.ProjectID == nil) || (d.ProjectID != nil && *d.ProjectID != tc.wantID) {
				t.Errorf("%s: id %v", name, d.ProjectID)
			}
		}
	}
}

func TestClientTimedOutFlag(t *testing.T) {
	hang := serve(t, func(c net.Conn, _ string) { time.Sleep(2 * time.Second) })
	_, oerr := (&Client{Socket: hang, Timeout: 100 * time.Millisecond}).Do(context.Background(), OpCreateProject, map[string]any{})
	if oerr == nil || oerr.Unsent || !oerr.TimedOut || !oerr.UnknownOutcome() {
		t.Fatalf("client deadline: %+v", oerr)
	}
	drop := serve(t, func(c net.Conn, _ string) {})
	_, oerr = (&Client{Socket: drop}).Do(context.Background(), OpCreateProject, map[string]any{})
	if oerr == nil || oerr.TimedOut || !oerr.UnknownOutcome() {
		t.Fatalf("dropped connection: %+v", oerr)
	}
	brokerTimeout := serve(t, func(c net.Conn, _ string) { c.Write([]byte("{\"ok\":false,\"code\":\"timeout\"}\n")) })
	_, oerr = (&Client{Socket: brokerTimeout}).Do(context.Background(), OpCreateProject, map[string]any{})
	if oerr == nil || oerr.Code != "timeout" || oerr.TimedOut || !oerr.UnknownOutcome() {
		t.Fatalf("broker timeout: %+v", oerr)
	}
}

// create_project gets 70 s (the broker's own limit is 60 s); the broker's
// limit stays shorter than the client's, and the web waits longer still.
func TestW3cDefaultTimeouts(t *testing.T) {
	want := map[string]time.Duration{OpCreateProject: 70 * time.Second, OpCreateGroup: 25 * time.Second, OpListGroups: 25 * time.Second,
		OpSetProvider: 20 * time.Second, OpDeleteProvider: 15 * time.Second, OpProviders: 8 * time.Second}
	brokerLimit := map[string]time.Duration{OpCreateProject: 60 * time.Second, OpCreateGroup: 20 * time.Second, OpListGroups: 20 * time.Second,
		OpSetProvider: 15 * time.Second, OpDeleteProvider: 10 * time.Second, OpProviders: 5 * time.Second}
	for op, d := range want {
		if defaultTimeouts[op] != d {
			t.Errorf("%s: %v want %v", op, defaultTimeouts[op], d)
		}
		if defaultTimeouts[op] <= brokerLimit[op] {
			t.Errorf("%s: client limit not above the broker's", op)
		}
	}
}

func TestRegistryViewDecodesProvidersAndOrphans(t *testing.T) {
	sock := serve(t, func(c net.Conn, _ string) {
		c.Write([]byte(`{"ok":true,"result":{"projects":[{"id":"a-app","path":"/r/a-app","kind":"registered","ai_context":false,"provider":"gl","gitlab":{"project":"team/a-app","project_id":5},"auto_merge":{"enabled":false}}],` +
			`"providers":[{"id":"gl","kind":"gitlab","base_url":"http://h:1","ssh_url":"ssh://git@h","token_ref":"gl-admin"}],` +
			`"auto_merge_global":{"enabled":false,"max_per_day":5},` +
			`"orphans":[{"provider":"gl","project":"team/x","project_id":null,"step":"project","at":"2026-11-09T12:00:00Z"}]}}` + "\n"))
	})
	v, oerr := (&Client{Socket: sock}).Projects(context.Background())
	if oerr != nil {
		t.Fatal(oerr)
	}
	if len(v.Providers) != 1 || v.Providers[0].SSHURL != "ssh://git@h" || v.Projects[0].Provider == nil || *v.Projects[0].Provider != "gl" ||
		len(v.Orphans) != 1 || v.Orphans[0].Step != "project" || v.Orphans[0].ProjectID != nil {
		t.Fatalf("%+v", v)
	}
}
