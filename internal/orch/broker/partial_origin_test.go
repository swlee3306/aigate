package broker

import (
	"encoding/json"
	"testing"
	"time"
)

// W3e E3 fix round C: the create_partial/link_partial detail gains `origin`
// (additive: a detail without it is an older broker's and reads as created).
func TestParsePartialDetailOrigin(t *testing.T) {
	base := func(extra string) json.RawMessage {
		return json.RawMessage(`{"provider":"gl","project":"team/app","project_id":5,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false` + extra + `}`)
	}
	for extra, want := range map[string]string{"": "created", `,"origin":"created"`: "created", `,"origin":"linked"`: "linked"} {
		d := parsePartialDetail(base(extra))
		if d == nil || d.Origin != want {
			t.Errorf("%q: %+v", extra, d)
		}
	}
	for _, bad := range []string{`,"origin":""`, `,"origin":"made"`, `,"origin":1`, `,"origin":null`, `,"origin":"linked","x":1`} {
		if d := parsePartialDetail(base(bad)); d != nil {
			t.Errorf("%q accepted: %+v", bad, d)
		}
	}
	// a GitHub path (case kept) is a valid project path of the detail
	gh := json.RawMessage(`{"provider":"gh","project":"SampleUser/Repo.v2","project_id":null,"step":"register","orphan_recorded":true,"orphan_dropped":false,"origin":"linked"}`)
	if d := parsePartialDetail(gh); d == nil || d.Project != "SampleUser/Repo.v2" {
		t.Fatalf("%+v", d)
	}
	if !ValidOrphanOrigin("") || !ValidOrphanOrigin("created") || !ValidOrphanOrigin("linked") || ValidOrphanOrigin("x") {
		t.Fatal("ValidOrphanOrigin")
	}
}

// W3f: the deletion ops have client time limits a little above the broker's own
// (10, 45, 60, 10 s), so a slow but successful delete is not an unknown outcome
// by orchd's clock; and the registry view carries remote_deleted_at.
func TestDeleteOpTimeoutsExceedTheBrokersAndViewCarriesStamp(t *testing.T) {
	for op, brokerLimit := range map[string]time.Duration{OpRemoveProject: 10 * time.Second, OpDeleteRemoteRepo: 45 * time.Second,
		OpRemoveDeployKey: 60 * time.Second, OpClearOrphan: 10 * time.Second} {
		d := defaultTimeouts[op]
		if d <= brokerLimit || d > brokerLimit+30*time.Second {
			t.Errorf("%s: client limit %v vs broker %v", op, d, brokerLimit)
		}
	}
	var p Project
	if err := json.Unmarshal([]byte(`{"id":"a","path":"/r/a","kind":"registered","origin":"created","remote_deleted_at":"2026-11-13T01:02:03Z","remote":null}`), &p); err != nil ||
		p.RemoteDeletedAt == nil || *p.RemoteDeletedAt != "2026-11-13T01:02:03Z" || p.Remote != nil {
		t.Fatalf("%+v %v", p, err)
	}
}
