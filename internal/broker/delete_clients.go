package broker

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"aigate/internal/registrymsg"
)

// W3f (F2): the delete side of the two provider clients. Every request still
// goes through the client's single `do` (allowlist first, token of the
// provider's own token_ref, redirects refused, no body reflection); this file
// only adds the four operations that use the delete rows of the allowlists:
//
//	GitHub  DELETE /repos/{owner}/{repo}            204
//	GitHub  GET    /repos/{owner}/{repo}/keys       200
//	GitHub  DELETE /repos/{owner}/{repo}/keys/{id}  204
//	GitLab  DELETE /projects/{id}                   202 (accepted; deletion may be delayed)
//	GitLab  GET    /projects/{id}/deploy_keys       200
//	GitLab  DELETE /projects/{id}/deploy_keys/{id}  204 (this project only)
//
// The success statuses are the documented ones for GitHub (docs: Delete a
// repository 204, Delete a deploy key 204). GitLab's page names no status for
// either delete: 202 and 204 are what its API source answers; anything else is
// NOT treated as success (the caller reports an uncertain outcome).
//
// Provenance handles. A destructive call is only made for something this client
// instance just read: LiveRepo records (path, id); DeleteRepo and ListDeployKeys
// need that record, and DeleteRepo consumes it (a second delete needs a second
// LiveRepo); RemoveDeployKey needs a key ListDeployKeys of this instance
// returned for the same live repository and consumes it. A copied or hand-built
// RepoInfo is refused before anything is sent.
//
// Encoding: the paths are built from the validated owner/repo (GitHub) or from
// numeric ids (GitLab); nothing needs percent-encoding and nothing is encoded.

type delState struct {
	mu     sync.Mutex
	live   map[string]int64 // normalised repository path -> id read by LiveRepo
	listed map[string]bool  // "path#keyid" of keys ListDeployKeys returned
}

func (d *delState) setLive(path string, id int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live == nil {
		d.live = map[string]int64{}
	}
	d.live[path] = id
}

func (d *delState) isLive(path string, id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	got, ok := d.live[path]
	return ok && got == id
}

// takeLive consumes the live handle: true only the first time.
func (d *delState) takeLive(path string, id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	got, ok := d.live[path]
	if !ok || got != id {
		return false
	}
	delete(d.live, path)
	return true
}

func (d *delState) setListed(path string, keys []DeployKeyInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listed = map[string]bool{}
	for _, k := range keys {
		d.listed[path+"#"+strconv.FormatInt(k.ID, 10)] = true
	}
}

func (d *delState) takeListed(path string, id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := path + "#" + strconv.FormatInt(id, 10)
	if !d.listed[k] {
		return false
	}
	delete(d.listed, k)
	return true
}

// keyBody reduces a public key line to "type base64" (the comment is dropped).
func keyBody(line string) (string, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", false
	}
	return f[0] + " " + f[1], true
}

type keyJSON struct {
	ID    *int64  `json:"id"`
	Title *string `json:"title"`
	Key   *string `json:"key"`
}

// decodeKeys turns one page into keys; any entry that lacks id, title or key
// makes the whole answer unusable (the list must be complete and exact before
// anything is removed on its basis).
func decodeKeys(raw []byte) ([]DeployKeyInfo, bool) {
	var list []keyJSON
	if !decodeFields(raw, &list) {
		return nil, false
	}
	out := make([]DeployKeyInfo, 0, len(list))
	for _, k := range list {
		if k.ID == nil || *k.ID <= 0 || k.Title == nil || k.Key == nil {
			return nil, false
		}
		body, ok := keyBody(*k.Key)
		if !ok {
			return nil, false
		}
		out = append(out, DeployKeyInfo{ID: *k.ID, Title: *k.Title, Body: body})
	}
	return out, true
}

// ---------- GitHub ----------

func ghKey(path string) string { return strings.ToLower(path) }

// LiveRepo: GET the repository and report id and full name as GitHub writes them.
func (a *GitHubAPI) LiveRepo(ctx context.Context, fullPath string) (RepoInfo, error) {
	owner, repo, ok := splitGH(fullPath)
	if !ok {
		return RepoInfo{}, ErrBadInput
	}
	raw, err := a.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, nil, nil, http.StatusOK)
	if err != nil {
		return RepoInfo{}, err
	}
	var j ghRepoJSON
	if !decodeFields(raw, &j) || j.ID == nil || *j.ID <= 0 || j.FullName == nil {
		return RepoInfo{}, ghBadResponse()
	}
	if _, _, ok := splitGH(*j.FullName); !ok {
		return RepoInfo{}, ghBadResponse()
	}
	a.del.setLive(ghKey(*j.FullName), *j.ID)
	return RepoInfo{ID: *j.ID, Path: *j.FullName}, nil
}

// DeleteRepo sends DELETE /repos/{owner}/{repo} once for the repository LiveRepo
// returned. Any answer but 204 is an error; the caller decides from the error
// whether the outcome is uncertain (see deleteNotSent).
func (a *GitHubAPI) DeleteRepo(ctx context.Context, repo RepoInfo) error {
	owner, name, ok := validGHRepoInfo(repo)
	if !ok || !a.del.takeLive(ghKey(repo.Path), repo.ID) {
		return ErrBadInput
	}
	_, err := a.do(ctx, http.MethodDelete, "/repos/"+owner+"/"+name, nil, nil, http.StatusNoContent)
	return err
}

// ListDeployKeys lists all deploy keys (pages of 100, page numbers only). A list
// that may be incomplete (page cap reached with a full page) is an error.
func (a *GitHubAPI) ListDeployKeys(ctx context.Context, repo RepoInfo) ([]DeployKeyInfo, error) {
	owner, name, ok := validGHRepoInfo(repo)
	if !ok || !a.del.isLive(ghKey(repo.Path), repo.ID) {
		return nil, ErrBadInput
	}
	var all []DeployKeyInfo
	for page := 1; page <= apiMaxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(apiPerPage)}, "page": {strconv.Itoa(page)}}
		raw, err := a.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/keys", q, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		keys, ok := decodeKeys(raw)
		if !ok {
			return nil, ghBadResponse()
		}
		all = append(all, keys...)
		if len(keys) < apiPerPage {
			a.del.setListed(ghKey(repo.Path), all)
			return all, nil
		}
	}
	return nil, ghBadResponse() // more than the cap: never decide on a partial list
}

// RemoveDeployKey sends DELETE /repos/{owner}/{repo}/keys/{id} once.
func (a *GitHubAPI) RemoveDeployKey(ctx context.Context, repo RepoInfo, key DeployKeyInfo) error {
	owner, name, ok := validGHRepoInfo(repo)
	if !ok || !a.del.isLive(ghKey(repo.Path), repo.ID) || key.ID <= 0 || !a.del.takeListed(ghKey(repo.Path), key.ID) {
		return ErrBadInput
	}
	_, err := a.do(ctx, http.MethodDelete, "/repos/"+owner+"/"+name+"/keys/"+strconv.FormatInt(key.ID, 10), nil, nil, http.StatusNoContent)
	return err
}

// ---------- GitLab ----------

// LiveRepo: GET the project by path and report id and path_with_namespace.
func (a *GitLabAPI) liveRepo(ctx context.Context, fullPath string) (RepoInfo, error) {
	i := strings.LastIndexByte(fullPath, '/')
	if i < 0 || !registrymsg.ValidGroupPath(fullPath[:i]) || !registrymsg.ValidName(fullPath[i+1:]) {
		return RepoInfo{}, ErrBadInput
	}
	raw, err := a.do(ctx, http.MethodGet, "/api/v4/projects/"+esc(fullPath), nil, nil, http.StatusOK)
	if err != nil {
		return RepoInfo{}, err
	}
	var j projectJSON
	if !decodeFields(raw, &j) || j.ID == nil || *j.ID <= 0 || j.PathWithNamespace == nil || !registrymsg.ValidRemotePath(*j.PathWithNamespace) {
		return RepoInfo{}, badResponse()
	}
	a.del.setLive(*j.PathWithNamespace, *j.ID)
	return RepoInfo{ID: *j.ID, Path: *j.PathWithNamespace}, nil
}

// deleteRepo sends DELETE /projects/{id} once (id: the one LiveRepo read).
func (a *GitLabAPI) deleteRepo(ctx context.Context, repo RepoInfo) error {
	ids, ok := idPath(repo.ID)
	if !ok || !a.del.takeLive(repo.Path, repo.ID) {
		return ErrBadInput
	}
	_, err := a.do(ctx, http.MethodDelete, "/api/v4/projects/"+ids, nil, nil, http.StatusAccepted)
	return err
}

func (a *GitLabAPI) listDeployKeys(ctx context.Context, repo RepoInfo) ([]DeployKeyInfo, error) {
	ids, ok := idPath(repo.ID)
	if !ok || !a.del.isLive(repo.Path, repo.ID) {
		return nil, ErrBadInput
	}
	var all []DeployKeyInfo
	for page := 1; page <= apiMaxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(apiPerPage)}, "page": {strconv.Itoa(page)}}
		raw, err := a.do(ctx, http.MethodGet, "/api/v4/projects/"+ids+"/deploy_keys", q, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		keys, ok := decodeKeys(raw)
		if !ok {
			return nil, badResponse()
		}
		all = append(all, keys...)
		if len(keys) < apiPerPage {
			a.del.setListed(repo.Path, all)
			return all, nil
		}
	}
	return nil, badResponse()
}

// removeDeployKey sends DELETE /projects/{id}/deploy_keys/{key_id} once: the key
// is taken off THIS project (GitLab keeps it for other projects that use it).
func (a *GitLabAPI) removeDeployKey(ctx context.Context, repo RepoInfo, key DeployKeyInfo) error {
	ids, ok := idPath(repo.ID)
	if !ok || !a.del.isLive(repo.Path, repo.ID) || key.ID <= 0 || !a.del.takeListed(repo.Path, key.ID) {
		return ErrBadInput
	}
	_, err := a.do(ctx, http.MethodDelete, "/api/v4/projects/"+ids+"/deploy_keys/"+strconv.FormatInt(key.ID, 10), nil, nil, http.StatusNoContent)
	return err
}

// deleteNotSent: err says nothing was sent (an input refused before the
// request, the allowlist, a missing token, the dial-time address policy), so a
// delete that failed this way changed nothing. A provider refusal that is a
// real answer is classified by refusedBeforeWrite; everything else after a
// request may have left is uncertain.
func deleteNotSent(err error) bool {
	switch ErrCode(err) {
	case CodeBadInput, CodeNotAllowlisted, CodeTokenRefUnknown, CodeBadURL:
		return ErrStatus(err) == 0
	}
	return false
}
