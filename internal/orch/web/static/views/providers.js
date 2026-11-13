// providers.js: GitLab and GitHub providers (#/providers) and the machinery of the
// other signed changes of the provider screens (a new group, a new project, a
// link to an existing repository, a remote attached to a project). Every change is SIGNED:
//
//   form -> POST .../propose (stores nothing) -> confirmation screen ->
//   passkey signature -> POST .../apply -> result
//
// The confirmation screen shows what will change in the browser's own words
// (the fields THIS page sent) next to the server's sentences, and offers the
// signature only when the server's proposal says the same thing: the echoed
// fields equal the ones sent, the server's `change` equals the SHA-256 the
// browser computes itself over the canonical JSON of those fields (the same
// bytes as Go's registrymsg), the message to sign carries that change, and
// for a new project, a link or an attach the deploy key line hashes to the signed
// deploy_key_sha256 and its fingerprint (computed here from the key blob, as
// ssh-keygen -l does) is the one the server shows, and for a GitHub project the
// chosen visibility is the one the proposal says. Otherwise there is a fixed
// warning and no sign button.
//
// There is NO secret input on these screens: a provider points at a token by
// NAME (token_ref) chosen from the token list of the token screen.
//
// Every string from the server is a text node (el()); the server's error words
// are never shown, only fixed sentences chosen by code. Nothing is stored in
// the browser, and this file never navigates or reads the clock.

import { el, inline, clear, findHostile } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';
import {
  signRegistryChange, checkRegistryMessage, sha256Hex, sha256HexBytes, hexToBytes, activeSigners, rememberedPasskey, supported, waText,
} from '../webauthn.js';

// --- constants shared with the server (TestConstantsAgreeWithWeb compares them) ---

export const PROVIDER_ID_RE = /^[a-z][a-z0-9-]{0,39}$/;
export const TOKEN_REF_RE = /^[a-z][a-z0-9-]{0,39}$/;
export const NAME_RE = /^[a-z][a-z0-9-]{1,39}$/;
export const HASH_RE = /^[0-9a-f]{64}$/;
export const DEPLOY_KEY_RE = /^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/]{16,1000}={0,2}(?: [!-~]{1,100})?$/;
export const NAME_MAX = 40;
export const MAX_GROUP_DEPTH = 4;
export const MAX_DESCRIPTION_RUNES = 200;
export const URL_MAX = 200;
export const TEMPLATES = ['basic', 'go'];
export const PUBLISH_MODES = ['write', 'read'];
// How an orphan record came to be: a half-made repository ('created') or an existing
// one that may hold this server's deploy key ('linked'). Absent = created (older server).
export const ORPHAN_ORIGINS = ['created', 'linked'];
export const PARTIAL_STEPS = ['project', 'first_commit', 'default_branch', 'protect', 'deploy_key', 'register'];
// The only string fields that may be "" (registrymsg's emptyOK).
export const EMPTY_OK = ['create_group.parent_path', 'create_project.description'];
export const ACTION_SET_PROVIDER = 'set_provider';
export const ACTION_DELETE_PROVIDER = 'delete_provider';
export const ACTION_CREATE_GROUP = 'create_group';
export const ACTION_CREATE_PROJECT = 'create_project';
export const ACTION_LINK_PROJECT = 'link_project';
export const ACTION_ATTACH_REMOTE = 'attach_remote';
// W3f: the four signed deletion actions (registrymsg.Action*). TestConstantsAgreeWithWeb compares them.
export const ACTION_REMOVE_PROJECT = 'remove_project';
export const ACTION_DELETE_REMOTE_REPO = 'delete_remote_repo';
export const ACTION_REMOVE_DEPLOY_KEY = 'remove_deploy_key';
export const ACTION_CLEAR_ORPHAN = 'clear_orphan';
export const DELETE_ACTIONS = [ACTION_REMOVE_PROJECT, ACTION_DELETE_REMOTE_REPO, ACTION_REMOVE_DEPLOY_KEY, ACTION_CLEAR_ORPHAN];
// remote_id of delete_remote_repo: 1 to 15 digits, no leading zero (registrymsg.ValidRemoteID).
export const REMOTE_ID_RE = /^[1-9][0-9]{0,14}$/;
// W3e: provider kinds (set_provider.kind), the visibility of a new GitHub repository
// (signed, no default), the GitHub address defaults and the name rules of a GitHub
// owner / repository. TestConstantsAgreeWithWeb compares all of them with registrymsg.
export const PROVIDER_KINDS = ['gitlab', 'github'];
export const VISIBILITIES = ['public', 'private'];
export const OPTIONAL_CREATE_FIELDS = ['visibility'];
export const GITHUB_API_URL = 'https://api.github.com';
export const GITHUB_SSH_URL = 'ssh://git@github.com';
export const GITHUB_LOGIN_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$/;
export const GITHUB_REPO_RE = /^[A-Za-z0-9._-]{1,100}$/;
export const MAX_REMOTE_SEGMENTS = 5;
export const MAX_REMOTE_SEGMENT_LEN = 100;
export const MAX_REMOTE_PATH_LEN = 255;

// Timeouts (ms). The server may take about 25 s for a provider or group write and
// about 90 s for a create (GitLab calls plus the automatic clone); a clone alone
// is limited to 120 s there. The browser must outwait all of them: if it gave up
// first, a call that succeeded would look like a failure. A timeout is never a
// "no": it is "unknown" and nothing is sent again.
export const PROPOSE_TIMEOUT_MS = 25000;
export const PROVIDER_TIMEOUT_MS = 30000;
export const CREATE_APPLY_TIMEOUT_MS = 130000;
export const CLONE_TIMEOUT_MS = 130000;
// A link or an attach reads the registry through the broker already while proposing (up to
// 15 s) and again when applying, and a link then clones: they outwait orchd like a create.
export const LINK_PROPOSE_TIMEOUT_MS = 30000;
export const LINK_APPLY_TIMEOUT_MS = 130000;
// W3f: the delete applies outwait orchd (broker op up to 70 s plus the local removal); the removal preview
// runs git and a size walk on the server (budget 25 s) and the signed proposals read the broker.
export const DELETE_APPLY_TIMEOUT_MS = 130000;
export const DELETE_PROPOSE_TIMEOUT_MS = 30000;
export const REMOVAL_PREVIEW_TIMEOUT_MS = 30000;
export const ATTACH_APPLY_TIMEOUT_MS = 130000;

const MAX_PREVIEW_LINES = 14;
const MAX_PREVIEW_CHARS = 600;
const MAX_KEY_LINE = 1300;
const MAX_ROWS = 200;
const KEY_PREFIX_CHARS = 48;

// The server's FIXED preview lines (internal/orch/api createPreview), compared whole:
// a description that happens to contain such words is on its own "설명:" line and
// can neither fake nor break the gate. TestConstantsAgreeWithWeb pins them.
export const PREVIEW_AI_ON = '[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다';
export const PREVIEW_AI_OFF = '이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)';
export const PREVIEW_PUBLISH_WRITE = '게시 권한 — 쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다';
export const PREVIEW_PUBLISH_READ = '게시 권한 — 읽기: 배포 키는 읽기 전용으로 등록합니다(이 서버는 브랜치를 올리지 못합니다)';
// The provider select fires a change per arrow key: the group list (a GitLab call
// with an hourly limit) is read once the select has been quiet this long, or on blur.
export const GROUP_SETTLE_MS = 400;
// The fixed preview lines of the W3e proposals (internal/orch/api createPreview, remotePreview),
// compared whole like the AI lines: a proposal for a public repository must say so exactly once.
export const PREVIEW_VIS_PUBLIC = '[중요] 공개 저장소로 만듭니다: 인터넷의 누구나 이 저장소의 코드를 읽을 수 있습니다';
export const PREVIEW_VIS_PRIVATE = '비공개 저장소로 만듭니다: 초대받은 사람만 볼 수 있습니다';
export const PREVIEW_ATTACH_PUSH_WARN = '[중요] 게시 권한이 쓰기입니다: 이 프로젝트의 로컬 저장소 기록(커밋 이력)이 이 원격으로 올라갈 수 있습니다. 원격이 공개 저장소라면 누구나 그 기록을 읽을 수 있습니다';
export const PREVIEW_KEY_WARN = '[중요] 이미 있는 저장소의 설정에 이 서버의 배포 키가 등록됩니다. 이 도구는 저장소를 만들거나 지우거나 다른 설정을 바꾸지 않습니다. 연결한 저장소는 이 도구로 지울 수 없고, 등록한 키는 삭제 화면의 "배포 키만 떼기"로 뗄 수 있습니다';

// W3f: marks of the server's fixed deletion lines (internal/orch/api deletePreview; TestConstantsAgreeWithWeb pins them).
export const PREVIEW_DEL_LEGACY_MARK = '처음부터 있던(legacy) 프로젝트입니다';
export const PREVIEW_DEL_IRREVERSIBLE_MARK = '[중요] 되돌릴 수 없습니다';
export const PREVIEW_DEL_REMOTE_MARK = '[중요] 원격 저장소 ';
export const PREVIEW_DEL_ALSO_MARK = '원격 삭제가 끝나면 이 서버의 복제본 폴더(';

const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);
const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);
const SCHEMA = {
  [ACTION_SET_PROVIDER]: { kind: 's', base_url: 's', ssh_url: 's', token_ref: 's' },
  [ACTION_DELETE_PROVIDER]: {},
  [ACTION_CREATE_GROUP]: { name: 's', path: 's', parent_path: 's' },
  [ACTION_CREATE_PROJECT]: { provider: 's', group_path: 's', template: 's', description: 's', ai_context: 'b', publish: 's', deploy_key_sha256: 's' },
  [ACTION_LINK_PROJECT]: { provider: 's', remote_path: 's', ai_context: 'b', publish: 's', deploy_key_sha256: 's' },
  [ACTION_ATTACH_REMOTE]: { provider: 's', remote_path: 's', publish: 's', deploy_key_sha256: 's' },
  [ACTION_REMOVE_PROJECT]: { scope: 's', legacy_ack: 'b', detach_key: 'b' },
  [ACTION_DELETE_REMOTE_REPO]: { remote_path: 's', remote_id: 's', confirm_name: 's', also_local: 'b' },
  [ACTION_REMOVE_DEPLOY_KEY]: { remote_path: 's' },
  [ACTION_CLEAR_ORPHAN]: { project: 's' },
};
// Fields that may be present or absent (registrymsg's optionalFields): `visibility` of a GitHub
// create. Whether it must be there is the server's call per provider kind; this page sends it
// for a GitHub provider and never for GitLab.
const OPTIONAL = { [ACTION_CREATE_PROJECT]: { visibility: 's' } };

// --- shape validators (ports of internal/registrymsg/names.go) ----------------
// The server is the authority. These only keep a bad value from costing a
// proposal; where this page is stricter than the server (IPv6 address
// literals are not offered) a proposal is simply not made.

export const validName = (s) => typeof s === 'string' && NAME_RE.test(s);

export function validGroupPath(s) {
  if (typeof s !== 'string') return false;
  const segs = s.split('/');
  return segs.length >= 1 && segs.length <= MAX_GROUP_DEPTH && segs.every(validName);
}

// validOwnerField: the SIGNED owner field of create_project (port of registrymsg.ValidOwnerField):
// a superset of a GitLab group path and a GitHub login. Which of them a provider
// accepts is the server's call (the broker op checks it before the signature).
export function validOwnerField(s) {
  if (typeof s !== 'string' || s.length < 1 || s.length > 100) return false;
  const segs = s.split('/');
  return segs.length <= MAX_GROUP_DEPTH + 1 && segs.every((g) => g !== '' && g !== '.' && g !== '..' && /^[A-Za-z0-9._-]+$/.test(g));
}

// validGitHubOwner: the signed owner of a GitHub create (port of registrymsg.ValidGitHubOwner):
// one login, case kept, no trailing hyphen.
export const validGitHubOwner = (s) => typeof s === 'string' && GITHUB_LOGIN_RE.test(s) && !s.endsWith('-');

// validGitHubRepoName: a GitHub repository name (port of registrymsg.ValidGitHubRepoName).
export function validGitHubRepoName(s) {
  if (typeof s !== 'string' || !GITHUB_REPO_RE.test(s) || s === '.' || s === '..') return false;
  return !(s.length >= 4 && s.slice(-4).toLowerCase() === '.git');
}

// validRemotePath: a remote repository path (port of registrymsg.ValidRemotePath): 2 to 5
// segments of [A-Za-z0-9._-], none empty, "." or "..", none ending in ".git", each at most 100
// bytes, the whole at most 255. Case is kept; per provider kind see validRemotePathForKind.
export function validRemotePath(s) {
  if (typeof s !== 'string' || s.length > MAX_REMOTE_PATH_LEN) return false;
  const segs = s.split('/');
  if (segs.length < 2 || segs.length > MAX_REMOTE_SEGMENTS) return false;
  return segs.every((g) => g !== '' && g !== '.' && g !== '..' && g.length <= MAX_REMOTE_SEGMENT_LEN && /^[A-Za-z0-9._-]+$/.test(g) && g.slice(-4).toLowerCase() !== '.git');
}

// validRemotePathForKind: the remote path a provider's client accepts (registrymsg.ValidRemotePathForKind).
export function validRemotePathForKind(kind, s) {
  if (!validRemotePath(s)) return false;
  const i = s.lastIndexOf('/');
  if (kind === 'github') return s.split('/').length === 2 && validGitHubLogin(s.slice(0, i)) && validGitHubRepoName(s.slice(i + 1));
  if (kind === 'gitlab') return i > 0 && validGroupPath(s.slice(0, i)) && validName(s.slice(i + 1));
  return false;
}
const validGitHubLogin = (s) => typeof s === 'string' && GITHUB_LOGIN_RE.test(s);

// validOwnerForKind: the provider-specific rule of the signed owner field (registrymsg.ValidOwnerForKind).
export function validOwnerForKind(kind, s) {
  if (kind === 'gitlab') return validGroupPath(s);
  if (kind === 'github') return validGitHubOwner(s);
  return false;
}

// validConfirmName: the typed repository name of a remote deletion (registrymsg.ValidConfirmName):
// one remote-path segment, case kept.
export function validConfirmName(s) {
  return typeof s === 'string' && s !== '' && s.length <= MAX_REMOTE_SEGMENT_LEN && !s.includes('/') && validRemotePath('x/' + s);
}

// validOrphanProject (registrymsg.ValidOrphanProject): a GitLab project (lowercase group path plus name)
// or any remote path.
export function validOrphanProject(s) {
  if (typeof s !== 'string') return false;
  const i = s.lastIndexOf('/');
  const gitlabShape = i >= 0 && validGroupPath(s.slice(0, i)) && validName(s.slice(i + 1)) && s.split('/').length <= MAX_GROUP_DEPTH + 1;
  return gitlabShape || validRemotePath(s);
}

// A project path as the registry holds it: 2 to 5 names (a group path and the project name).
export function validProjectPath(s) {
  if (typeof s !== 'string') return false;
  const segs = s.split('/');
  return segs.length >= 2 && segs.length <= MAX_GROUP_DEPTH + 1 && segs.every(validName);
}

const BAD_DESC_CHAR = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Co}\p{Cs}�]/u;

// validDescription: "" or at most 200 CODE POINTS (not UTF-16 units) without
// control, format (bidi, zero-width), line/paragraph separator or private-use characters.
export function validDescription(s) {
  if (typeof s !== 'string') return false;
  if ([...s].length > MAX_DESCRIPTION_RUNES) return false;
  // Also what this page itself refuses to show on a signing screen (variation selectors,
  // Hangul fillers, combining grapheme joiner, ...): Go is looser here, the page is stricter,
  // so the form never makes a proposal whose own preview the page would then reject.
  return !BAD_DESC_CHAR.test(s) && findHostile(s) === null;
}

const HOST_LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const IPV4_RE = /^(0|[1-9][0-9]{0,2})(\.(0|[1-9][0-9]{0,2})){3}$/;

function validPort(p) {
  if (p === '' || p.length > 5 || p[0] === '0' || !/^[0-9]+$/.test(p)) return false;
  const n = Number(p);
  return n >= 1 && n <= 65535;
}

function validHostname(h) {
  if (h === '' || h.length > 253) return false;
  const labels = h.split('.');
  if (!labels.every((l) => HOST_LABEL_RE.test(l))) return false;
  // a name whose last label is all digits can only be a canonical dotted quad
  if (/^[0-9]+$/.test(labels[labels.length - 1])) {
    return IPV4_RE.test(h) && h.split('.').every((x) => Number(x) <= 255);
  }
  return true;
}

function validAuthority(a) {
  if (a === '' || a.startsWith('[')) return false;
  let host = a;
  const i = a.indexOf(':');
  if (i >= 0) {
    host = a.slice(0, i);
    if (!validPort(a.slice(i + 1))) return false;
  }
  return validHostname(host);
}

export function validBaseUrl(s) {
  if (typeof s !== 'string' || s.length > URL_MAX) return false;
  if (s.startsWith('http://')) return validAuthority(s.slice(7));
  if (s.startsWith('https://')) return validAuthority(s.slice(8));
  return false;
}

// githubBaseUrlProblem: null when s can be the API address of a GitHub provider, else why not:
// 'shape' (not http(s)://host[:port]), 'web_host' (github.com is the web site, the API of
// github.com is api.github.com), 'http' (plain http only for an address literal of a private or
// tailnet GitHub Enterprise Server: the server decides which). Like the broker's rule.
export function githubBaseUrlProblem(s) {
  if (!validBaseUrl(s)) return 'shape';
  const host = s.replace(/^https?:\/\//, '').split(':')[0];
  if (host === 'github.com' || host === 'www.github.com') return 'web_host';
  if (s.startsWith('http://') && !IPV4_RE.test(host)) return 'http';
  return null;
}

export function validSshUrl(s) {
  if (typeof s !== 'string' || s.length > URL_MAX || !s.startsWith('ssh://git@')) return false;
  return validAuthority(s.slice(10));
}

// --- canonical change (the same bytes as Go's registrymsg.CanonicalChange) ----

export class ChangeError extends Error {
  constructor(why) {
    super(why);
    this.name = 'ChangeError';
  }
}

// A string whose canonical JSON could differ between Go and JSON.stringify:
// control characters, DEL, U+2028/9 and lone surrogates.
function checkString(s) {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x20 || c === 0x7f || c === 0x2028 || c === 0x2029) throw new ChangeError('control character');
    if (c >= 0xd800 && c <= 0xdbff) {
      const d = s.charCodeAt(i + 1);
      if (!(d >= 0xdc00 && d <= 0xdfff)) throw new ChangeError('lone surrogate');
      i++;
    } else if (c >= 0xdc00 && c <= 0xdfff) {
      throw new ChangeError('lone surrogate');
    }
  }
}

function checkField(action, name, v) {
  const ok = (cond) => { if (!cond) throw new ChangeError('bad ' + name); };
  switch (action + '.' + name) {
    case ACTION_SET_PROVIDER + '.kind': return ok(v === 'gitlab' || v === 'github');
    case ACTION_SET_PROVIDER + '.base_url': return ok(validBaseUrl(v));
    case ACTION_SET_PROVIDER + '.ssh_url': return ok(validSshUrl(v));
    case ACTION_SET_PROVIDER + '.token_ref': return ok(TOKEN_REF_RE.test(v));
    case ACTION_CREATE_GROUP + '.name': case ACTION_CREATE_GROUP + '.path': return ok(validName(v));
    case ACTION_CREATE_GROUP + '.parent_path': return ok(v === '' || (validGroupPath(v) && v.split('/').length < MAX_GROUP_DEPTH));
    case ACTION_CREATE_PROJECT + '.provider': return ok(PROVIDER_ID_RE.test(v));
    case ACTION_CREATE_PROJECT + '.group_path': return ok(validOwnerField(v));
    case ACTION_CREATE_PROJECT + '.template': return ok(TEMPLATES.includes(v));
    case ACTION_CREATE_PROJECT + '.description': return ok(validDescription(v));
    case ACTION_CREATE_PROJECT + '.publish': return ok(PUBLISH_MODES.includes(v));
    case ACTION_CREATE_PROJECT + '.deploy_key_sha256': return ok(HASH_RE.test(v));
    case ACTION_CREATE_PROJECT + '.visibility': return ok(VISIBILITIES.includes(v));
    case ACTION_LINK_PROJECT + '.provider': case ACTION_ATTACH_REMOTE + '.provider': return ok(PROVIDER_ID_RE.test(v));
    case ACTION_LINK_PROJECT + '.remote_path': case ACTION_ATTACH_REMOTE + '.remote_path': return ok(validRemotePath(v));
    case ACTION_LINK_PROJECT + '.publish': case ACTION_ATTACH_REMOTE + '.publish': return ok(PUBLISH_MODES.includes(v));
    case ACTION_LINK_PROJECT + '.deploy_key_sha256': case ACTION_ATTACH_REMOTE + '.deploy_key_sha256': return ok(HASH_RE.test(v));
    case ACTION_REMOVE_PROJECT + '.scope': return ok(v === 'local');
    case ACTION_DELETE_REMOTE_REPO + '.remote_path': case ACTION_REMOVE_DEPLOY_KEY + '.remote_path': return ok(validRemotePath(v));
    case ACTION_DELETE_REMOTE_REPO + '.remote_id': return ok(REMOTE_ID_RE.test(v));
    case ACTION_DELETE_REMOTE_REPO + '.confirm_name': return ok(validConfirmName(v));
    case ACTION_CLEAR_ORPHAN + '.project': return ok(validOrphanProject(v));
    default: return ok(false);
  }
}

// canonicalChange returns {"action":..,"fields":{..sorted..},"subject":..}: no
// whitespace, no HTML escaping, only strings and booleans. It refuses
// whatever registrymsg refuses.
export function canonicalChange(action, subject, fields) {
  if (typeof action !== 'string' || !has(SCHEMA, action)) throw new ChangeError('unknown action');
  const spec = SCHEMA[action];
  const subjectOk = action === ACTION_CREATE_PROJECT || action === ACTION_LINK_PROJECT ? validName(subject) : typeof subject === 'string' && PROVIDER_ID_RE.test(subject);
  if (!subjectOk) throw new ChangeError('bad subject');
  if (fields === null || typeof fields !== 'object' || Array.isArray(fields)) throw new ChangeError('bad fields');
  const proto = Object.getPrototypeOf(fields);
  if (proto !== Object.prototype && proto !== null) throw new ChangeError('bad fields');
  const names = Object.keys(fields);
  const optional = has(OPTIONAL, action) ? OPTIONAL[action] : {};
  const given = names.filter((n) => has(optional, n)).length;
  // every required field exactly once, the optional ones as given, nothing else
  if (names.length - given !== Object.keys(spec).length) throw new ChangeError('fields do not match the action');
  for (const name of names) {
    if (!has(spec, name) && !has(optional, name)) throw new ChangeError('unknown field');
    const v = fields[name];
    if (has(spec, name) && spec[name] === 'b') {
      if (typeof v !== 'boolean') throw new ChangeError('wrong type');
      continue;
    }
    if (typeof v !== 'string' || (v === '' && !EMPTY_OK.includes(action + '.' + name))) throw new ChangeError('wrong type');
    checkString(v);
    checkField(action, name, v);
  }
  names.sort();
  const body = names.map((n) => JSON.stringify(n) + ':' + JSON.stringify(fields[n])).join(',');
  return '{"action":' + JSON.stringify(action) + ',"fields":{' + body + '},"subject":' + JSON.stringify(subject) + '}';
}

// computeChange is the `change` hash of a change: lowercase hex SHA-256 of
// canonicalChange. Async because WebCrypto is.
export async function computeChange(action, subject, fields) {
  return sha256Hex(canonicalChange(action, subject, fields));
}

// --- the deploy key: the signed hash and the fingerprint shown -----------------

// deployKeyHash is what the server signs for the key: lowercase hex SHA-256 of
// the UTF-8 text of the one-line public key, exactly as the server hashes it.
export const deployKeyHash = (line) => sha256Hex(line);

// sshFingerprint is `ssh-keygen -l` form: "SHA256:" and the unpadded base64 of
// the SHA-256 of the key blob (the decoded second field of the line). null when
// the line has no strictly valid blob.
export async function sshFingerprint(line) {
  if (typeof line !== 'string') return null;
  const parts = line.split(' ');
  if (parts.length < 2 || !/^[A-Za-z0-9+/]{16,1000}={0,2}$/.test(parts[1])) return null;
  let bin;
  try {
    bin = atob(parts[1]);
  } catch {
    return null;
  }
  if (btoa(bin) !== parts[1]) return null; // no stray bits: the blob re-encodes to the same text
  const blob = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) blob[i] = bin.charCodeAt(i);
  const digest = hexToBytes(await sha256HexBytes(blob));
  let s = '';
  for (const b of digest) s += String.fromCharCode(b);
  return 'SHA256:' + btoa(s).replace(/=+$/, '');
}

// --- fixed texts -------------------------------------------------------------

// ERROR_TEXT: one fixed sentence per code the W3c endpoints can answer
// (w3c-error-codes.json is generated from the server's source). The server's
// own words are never shown.
export const ERROR_TEXT = {
  bad_request: '요청 형식이 올바르지 않아 거부되었습니다. 변경은 적용되지 않았습니다.',
  not_signed: '서명이 없는 요청이라 거부되었습니다. 변경은 적용되지 않았습니다.',
  bad_signature: '서명이 올바르지 않아 거부되었습니다. 변경은 적용되지 않았습니다.',
  unknown_signer: '등록되지 않은 서명자의 서명이라 거부되었습니다. 변경은 적용되지 않았습니다.',
  stale_signature: '서명이 만료되었습니다(5분 안에만 쓸 수 있습니다). 변경은 적용되지 않았습니다. 처음부터 다시 진행하세요.',
  signers_unavailable: '서버의 서명자 목록을 쓸 수 없어 처리하지 못했습니다. 변경은 적용되지 않았습니다. 관리자에게 알려 주세요.',
  request_reused: '이미 사용된 요청이라 거부되었습니다. 변경은 다시 적용되지 않았습니다. 목록을 확인하고 필요하면 새로 시작하세요.',
  change_mismatch: '서명한 내용과 서버가 받은 내용이 달라 거부되었습니다. 변경은 적용되지 않았습니다.',
  path_not_allowed: '이 프로젝트 폴더는 허용된 저장소 폴더 바로 아래에 만들 수 없습니다. 관리자가 저장소 폴더 설정을 확인해야 합니다. 변경은 적용되지 않았습니다.',
  project_exists: '같은 ID 또는 같은 경로의 프로젝트가 이미 등록되어 있습니다. 변경은 적용되지 않았습니다.',
  project_unknown: '등록되지 않은 프로젝트입니다. 목록을 새로 읽어 확인하세요.',
  project_rate_limited: '요청이 너무 잦아 한도에 걸렸습니다(시간당 횟수 제한). 잠시 후 다시 시도하세요.',
  broker_rate_limited: '서버 전체의 시간당 생성 한도를 넘었습니다. 잠시 후 다시 시도하세요.',
  registry_unavailable: '프로젝트 등록부를 쓸 수 없어 처리하지 못했습니다. 관리자에게 알려 주세요.',
  tokens_unavailable: '토큰 저장소를 쓸 수 없어 처리하지 못했습니다. 관리자에게 알려 주세요.',
  broker_unavailable: '등록부 서비스에 연결하지 못했습니다. 변경은 적용되지 않았습니다. 잠시 후 다시 시도하세요.',
  broker_error: '등록부 서비스가 예상하지 못한 응답을 했습니다. 변경이 적용되었는지 목록에서 확인하세요.',
  provider_unknown: '등록되지 않은 제공자입니다. 변경은 적용되지 않았습니다. 목록을 새로 읽어 확인하세요.',
  provider_exists: '같은 id의 제공자가 이미 있습니다. 변경은 적용되지 않았습니다.',
  provider_in_use: '이 제공자를 쓰는 프로젝트가 있어 삭제하거나 API 주소·ssh 주소를 바꿀 수 없습니다. 변경은 적용되지 않았습니다.',
  bad_url: '서버가 이 제공자 주소를 허용하지 않았습니다(접속 대상이 허용 범위 밖이거나 주소가 맞지 않습니다). 변경은 적용되지 않았습니다.',
  token_ref_unknown: '제공자가 쓸 토큰이 없거나, 제공자와 종류가 맞지 않거나, 쓸 수 없습니다. 토큰 화면에서 확인하세요. 변경은 적용되지 않았습니다.',
  provider_unreachable: '제공자(GitLab/GitHub)에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요. 프로젝트를 만드는 중이었다면 목록의 알림도 확인하세요.',
  gitlab_refused: 'GitLab이 요청을 받아들이지 않았습니다. 토큰 권한과 GitLab 설정을 확인하세요. 프로젝트를 만드는 중이었다면 목록의 알림도 확인하세요.',
  group_unknown: '제공자에 그런 그룹(소유자)이 없습니다. 목록을 다시 읽어 고르세요. 변경은 적용되지 않았습니다.',
  group_exists: '같은 경로의 그룹이 이미 있습니다. 변경은 적용되지 않았습니다.',
  project_name_taken: '같은 이름의 프로젝트가 이미 등록되어 있거나 제공자에 있습니다. 기존 프로젝트는 이 화면에서 가져오지 않습니다("기존 저장소 연결"을 쓰세요). 변경은 적용되지 않았습니다.',
  template_unknown: '알 수 없는 템플릿입니다. 변경은 적용되지 않았습니다.',
  path_exists: '이 서버의 저장소 폴더에 같은 이름이 이미 있어 프로젝트를 만들지 않았습니다. 관리자가 그 폴더를 확인해야 합니다. 변경은 적용되지 않았습니다.',
  deploy_key_mismatch: '서명한 배포 키와 지금 서버의 배포 키가 다릅니다. 처음부터 다시 시작해 새로 서명하세요. 변경은 적용되지 않았습니다.',
  deploy_key_unavailable: '서버의 배포 키 공개 키 파일(.pub)을 읽을 수 없습니다. 관리자가 확인해야 합니다. 변경은 적용되지 않았습니다.',
  policy_class_missing: '서버 정책에 registered 클래스가 없어 새 프로젝트를 만들 수 없습니다. 관리자가 정책에 한 번 추가해야 합니다. 변경은 적용되지 않았습니다.',
  create_partial: '제공자(GitLab/GitHub)에 일부만 만들어졌습니다. 일부만 만들어진 것은 등록되지 않아 삭제 화면으로 지울 수 없고 이 도구는 삭제하지 않습니다. 제공자 사이트에서 직접 정리한 뒤 다시 만드세요.',
  outcome_unknown: '변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 목록을 다시 읽어 확인하고, 프로젝트라면 제공자 사이트(GitLab/GitHub)와 알림도 확인한 뒤 필요하면 다시 변경하세요.',
  not_clonable: '이 프로젝트는 제공자 연결이 없어 복제할 주소를 만들 수 없습니다.',
  host_not_pinned: '이 제공자 서버의 호스트 키가 서버에 고정되어 있지 않아(known_hosts) 복제하지 않았습니다. 관리자가 확인해야 합니다.',
  already_cloned: '이 프로젝트 폴더가 이미 있어 복제하지 않았습니다.',
  clone_in_progress: '이 프로젝트를 이미 복제하는 중입니다. 잠시 뒤 목록을 새로 읽어 확인하세요.',
  clone_timeout: '복제 시간이 초과되었습니다. 만들다 만 폴더는 지웠습니다. 다시 복제할 수 있습니다.',
  clone_failed: '복제에 실패했습니다. 만들다 만 폴더는 지웠습니다. 배포 키 권한과 제공자 연결을 확인한 뒤 다시 복제하세요.',
  clone_unavailable: '서버에 복제 설정(배포 키와 known_hosts)이 없어 복제할 수 없습니다. 관리자가 확인해야 합니다.',
  git_missing: '서버에서 git 실행 파일을 찾지 못해 복제할 수 없습니다. 관리자가 확인해야 합니다.',
  ssh_missing: '서버에서 ssh 실행 파일을 찾지 못해 복제할 수 없습니다. 관리자가 확인해야 합니다.',
  // W3e: GitHub, link and attach (every code orchd can answer on those paths; sentences are
  // fixed and never carry the broker's or the provider's words)
  github_refused: 'GitHub이 요청을 받아들이지 않았습니다. 토큰 권한(저장소 만들기·배포 키)과 GitHub 설정을 확인하세요. 저장소를 만드는 중이었다면 목록의 알림도 확인하세요.',
  provider_rate_limited: '제공자(GitHub/GitLab)가 요청 한도를 넘었다고 응답했습니다. 한참 뒤에 다시 시도하세요. 자동으로 다시 시도하지 않았습니다.',
  not_supported_for_provider: '이 제공자 종류에서는 지원하지 않는 작업입니다(예: GitHub에서는 새 그룹을 만들 수 없고 기존 계정이나 조직을 고릅니다). 변경은 적용되지 않았습니다.',
  visibility_required: 'GitHub 저장소는 공개 또는 비공개를 반드시 골라야 합니다(기본값이 없습니다). 변경은 적용되지 않았습니다.',
  visibility_not_allowed: 'GitLab 프로젝트에는 공개 여부를 정할 수 없습니다(항상 비공개로 만들어집니다). 변경은 적용되지 않았습니다.',
  repo_not_found: '연결하려는 저장소를 제공자에서 찾지 못했습니다. 경로를 확인하고, 토큰이 그 저장소를 볼 수 있는지 확인하세요. 변경은 적용되지 않았습니다.',
  already_linked: '이미 원격에 연결된 프로젝트이거나, 같은 원격 저장소가 이미 다른 프로젝트에 연결되어 있습니다. 변경은 적용되지 않았습니다.',
  static_remote_wins: '이 프로젝트는 서버 설정(--git-remote-for)에 원격이 이미 정해져 있어 그것이 우선합니다. 여기서 원격을 연결해도 효과가 없어 연결하지 않았습니다.',
  key_expired: '제안할 때 만든 저장소 전용 배포 키가 만료되었거나(15분) 서버가 다시 시작되어 사라졌습니다. 오래된 내용으로는 서명하지 않습니다: 처음부터 다시 제안하세요. 변경은 적용되지 않았습니다.',
  key_staging_failed: '서버가 이 저장소 전용 배포 키를 준비하지 못했습니다(같은 이름의 키 폴더가 이미 있거나 서버 설정 문제입니다). 관리자가 확인해야 합니다. 변경은 적용되지 않았습니다.',
  key_unavailable: '이 프로젝트의 저장소 전용 배포 키 파일을 쓸 수 없어(없거나, 권한이 맞지 않거나, 옮기지 못했습니다) 복제와 게시를 할 수 없습니다. 프로젝트는 이미 등록되었을 수 있습니다. 관리자가 확인해야 합니다.',
  // W3f: deletion (every code orchd's delete endpoints answer; fixed sentences, never the server's words)
  project_busy: '이 프로젝트에 대기 중이거나 실행 중이거나 게시·승인 대기 중인 작업이 있어 지울 수 없습니다. 작업이 끝나거나 취소된 뒤 다시 시도하세요. 변경은 적용되지 않았습니다.',
  busy_check_failed: '진행 중인 작업이 있는지 확인하지 못해 지우지 않았습니다. 잠시 후 다시 시도하세요. 변경은 적용되지 않았습니다.',
  local_unsafe: '서버의 복제본 폴더가 이 도구가 지울 수 있는 모양이 아니어서(심볼릭 링크이거나 .git 폴더가 없거나 허용된 위치가 아님) 아무것도 지우지 않았습니다. 서버 관리자가 확인해야 합니다.',
  key_unavailable_for_removal: '이 프로젝트의 배포 키 공개 키를 서버에서 읽을 수 없어 이 도구로 뗄 수 없습니다. 제공자 화면의 저장소 설정(Deploy keys)에서 제목이 orchd로 시작하는 키를 직접 제거하세요. 변경은 적용되지 않았습니다.',
  no_remote: '이 프로젝트에는 연결된 원격 저장소가 없어 뗄 배포 키가 없습니다. 변경은 적용되지 않았습니다.',
  delete_partial_local: '서버의 파일 지우기가 일부만 끝났습니다. 항목별 상태를 확인하세요. 자동으로 다시 시도하지 않았습니다.',
  delete_rate_limited: '서버 전체의 삭제 횟수 한도(시간당·하루)에 도달했습니다. 나중에 다시 시도하세요. 변경은 적용되지 않았습니다.',
  busy: '같은 프로젝트나 저장소에 대한 다른 변경이 진행 중입니다. 잠시 후 다시 시도하세요. 변경은 적용되지 않았습니다.',
  orphan_unknown: '그런 고아 기록이 없습니다(이미 지워졌을 수 있습니다). 목록을 다시 읽어 확인하세요. 변경은 적용되지 않았습니다.',
  repo_being_removed: '이 프로젝트의 서버 폴더를 지우는 중이라 지금은 작업을 내거나 복제할 수 없습니다. 잠시 후 다시 시도하세요.',
  legacy_ack_required: '처음부터 있던(legacy) 프로젝트는 두 번째 확인이 있어야 지울 수 있습니다. 변경은 적용되지 않았습니다. 처음부터 다시 진행하세요.',
  origin_unknown: '이 도구가 만든 저장소인지 알 수 없는 프로젝트라 원격 저장소는 지울 수 없습니다. 필요하면 GitHub·GitLab 화면에서 직접 지우세요. 변경은 적용되지 않았습니다.',
  not_created_by_tool: '이 도구가 만들지 않고 연결만 한 저장소라 원격 저장소는 지울 수 없습니다. 연결한 저장소에서는 이 서버의 배포 키만 뗄 수 있습니다. 변경은 적용되지 않았습니다.',
  remote_changed: '등록된 원격 저장소 정보와 요청이 다르거나, 제공자의 저장소가 바뀌었거나, 이미 원격이 없습니다. 목록을 다시 읽어 확인하세요. 변경은 적용되지 않았습니다.',
  confirm_mismatch: '입력한 저장소 이름이 등록된 저장소 이름과 글자 그대로(대소문자 포함) 같지 않습니다. 변경은 적용되지 않았습니다.',
  delete_partial: '원격 저장소 삭제가 일부만 처리되었거나 결과를 알 수 없습니다. 저장소가 지워졌을 수도, 아닐 수도 있습니다. 자동으로 다시 시도하지 않았고 서버의 복제본도 지우지 않았습니다. GitHub·GitLab 화면에서 확인하세요: 저장소가 아직 있으면 "원격 저장소 지우기"를 새로 서명해 다시 하고(서버가 저장소 번호와 이름을 다시 확인합니다), 이미 없으면 "복제본과 등록 지우기"로 서버의 항목을 정리하세요.',
  key_not_found: '제공자의 저장소에서 이 서버의 배포 키(제목이 orchd로 시작하고 공개 키가 같은 것)를 찾지 못했습니다. 이미 제거되었을 수 있습니다. 변경은 적용되지 않았습니다.',
  remove_refused: '제공자가 삭제 요청을 받아들이지 않았거나 대상이 하나로 정해지지 않아 아무것도 지우지 않았습니다. 토큰 권한과 제공자 화면을 확인하세요.',
  link_partial: '이미 있던 저장소에 이 서버의 배포 키가 붙었을 수 있습니다. 저장소는 지우지 마세요. 제공자 사이트에서 이 서버의 배포 키(제목이 orchd로 시작)만 직접 제거한 뒤 다시 시도하세요.',
};

// GITHUB_TEXT: the sentences that name the provider, in GitHub's words. Used where the screen
// knows the provider is a GitHub one (ERROR_TEXT is the provider-neutral baseline, which is also
// what GitLab screens show).
export const GITHUB_TEXT = {
  provider_in_use: ERROR_TEXT.provider_in_use,
  bad_url: '서버가 이 GitHub API 주소를 허용하지 않았습니다(접속 대상이 허용 범위 밖이거나 주소가 맞지 않습니다). 변경은 적용되지 않았습니다.',
  provider_unreachable: 'GitHub에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요. 서버의 방화벽이 GitHub을 허용하는지도 확인하세요. 저장소를 만드는 중이었다면 목록의 알림도 확인하세요.',
  group_unknown: 'GitHub에 그런 소유자(계정 또는 조직)가 없습니다. 소유자 목록을 다시 읽어 고르세요. 변경은 적용되지 않았습니다.',
  project_name_taken: '같은 이름의 저장소가 이미 등록되어 있거나 GitHub에 있습니다. 기존 저장소는 이 화면에서 가져오지 않습니다("기존 저장소 연결"을 쓰세요). 변경은 적용되지 않았습니다.',
  create_partial: 'GitHub에 일부만 만들어졌습니다. 일부만 만들어진 저장소는 등록되지 않아 삭제 화면으로 지울 수 없고 이 도구는 삭제하지 않습니다. GitHub에서 직접 정리한 뒤 다시 만드세요.',
  host_not_pinned: 'github.com 서버의 호스트 키가 서버에 고정되어 있지 않아(known_hosts) 복제하지 않았습니다. 관리자가 GitHub의 공식 지문과 비교해 넣어야 합니다.',
  clone_failed: '복제에 실패했습니다. 만들다 만 폴더는 지웠습니다. 저장소 전용 배포 키와 GitHub 연결(방화벽 포함)을 확인한 뒤 다시 복제하세요.',
};

const STATUS_TEXT = {
  403: '이 변경을 할 권한이 없습니다.',
  404: '찾을 수 없습니다. 목록을 새로 읽어 확인하세요.',
  429: ERROR_TEXT.project_rate_limited,
};

// failureText: the fixed sentence for an ApiError (kind: the provider kind of the screen, when it
// is known, picks GitHub's words). Unknown codes fall back to the HTTP status, never to the
// server's message.
export function failureText(err, kind) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (kind === 'github' && has(GITHUB_TEXT, code)) return GITHUB_TEXT[code];
  if (has(ERROR_TEXT, code)) return ERROR_TEXT[code];
  const st = err && typeof err.status === 'number' ? err.status : 0;
  if (has(STATUS_TEXT, st)) return STATUS_TEXT[st];
  if (st >= 500) return '서버에서 오류가 발생했습니다. 변경이 적용되었는지 목록에서 확인하세요.';
  return '요청을 처리하지 못했습니다. 목록을 새로 읽어 확인하세요.';
}

// isUnknown: the call may or may not have changed something: a timeout or network
// failure (status 0), the server's outcome_unknown, or a server/gateway error
// (5xx) that carries no code of the table (a proxy answered, not orchd). Never
// retried, never signed again; the page re-reads its lists and claims nothing.
export const isUnknown = (err) => {
  if (!err) return false;
  if (err.status === 0 || err.code === 'outcome_unknown') return true;
  const code = typeof err.code === 'string' ? err.code : '';
  return typeof err.status === 'number' && err.status >= 500 && !has(ERROR_TEXT, code);
};

// CLONE_TEXT: what a clone result or a failed clone call says, by code.
export const CLONE_TEXT = {
  host_not_pinned: ERROR_TEXT.host_not_pinned,
  clone_failed: ERROR_TEXT.clone_failed,
  clone_timeout: ERROR_TEXT.clone_timeout,
  clone_unavailable: ERROR_TEXT.clone_unavailable,
  not_clonable: ERROR_TEXT.not_clonable,
  path_not_allowed: '이 프로젝트 폴더는 허용된 저장소 폴더 바로 아래에 있지 않아 복제하지 않았습니다. 관리자가 확인해야 합니다.',
  clone_in_progress: ERROR_TEXT.clone_in_progress,
  registry_unavailable: ERROR_TEXT.registry_unavailable,
  broker_unavailable: ERROR_TEXT.broker_unavailable,
  git_missing: ERROR_TEXT.git_missing,
  ssh_missing: ERROR_TEXT.ssh_missing,
  already_cloned: ERROR_TEXT.already_cloned,
  check: '프로젝트는 만들어졌지만 이 서버에 같은 이름의 폴더가 이미 있어 복제하지 않았습니다. 그 폴더가 이 프로젝트의 것인지 확인이 필요합니다(관리자 확인).',
  unknown: '복제 결과를 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 목록을 다시 읽어 복제 여부를 확인하세요.',
  other: '복제하지 못했습니다. 잠시 후 다시 복제하거나 관리자에게 알려 주세요.',
};

export function cloneText(code, kind) {
  if (kind === 'github' && typeof code === 'string' && has(GITHUB_TEXT, code) && has(CLONE_TEXT, code)) return GITHUB_TEXT[code];
  return typeof code === 'string' && has(CLONE_TEXT, code) && code !== 'unknown' ? CLONE_TEXT[code] : CLONE_TEXT.other;
}

const TEXT = {
  mismatch: '서버가 보낸 변경 내용이 화면의 내용과 맞지 않아 서명할 수 없습니다. 아무것도 서명하거나 보내지 않았습니다. 처음부터 다시 하세요. 계속 같으면 관리자에게 알려 주세요.',
  noPasskeySession: '패스키로 로그인한 세션에서만 웹 서명을 할 수 있습니다.',
  noSigner: '로그인한 서명자의 활성 패스키를 찾지 못했습니다.',
  signersFailed: '패스키 목록을 불러오지 못했습니다. 잠시 후 다시 시도하세요.',
  connect: '서버에 연결하지 못했습니다. 아무것도 서명하거나 보내지 않았습니다.',
  proposeFailed: '변경 내용을 만들지 못했습니다. 아무것도 서명하거나 보내지 않았습니다. 잠시 후 다시 시도하세요.',
  aiOn: 'AI 전송 허용: 예. 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있습니다.',
  waiting: '패스키 확인을 기다리는 중…',
  sending: '서버로 변경을 보내는 중…',
};

// --- step list of a project creation (fixed words, never the server's) ---------

const STEP_ORDER = ['group', 'project', 'first_commit', 'protect', 'deploy_key', 'register', 'clone'];
const STEP_LABEL = {
  group: '그룹 확인', project: '프로젝트 만들기', first_commit: '첫 커밋과 기본 브랜치', protect: 'main 브랜치 보호',
  deploy_key: '배포 키 등록', register: '이 서버에 등록', clone: '이 서버에 복제',
};
// A GitHub create says "owner" and "repository" (and that the protection depends on the plan).
const STEP_LABEL_GITHUB = {
  ...STEP_LABEL, group: '소유자 확인', project: '저장소 만들기', protect: 'main 브랜치 보호(공개 저장소 또는 유료 요금제)', deploy_key: '저장소 전용 배포 키 등록',
};
const STATUS_LABEL = { done: '완료', failed: '실패', skipped: '실행 안 됨' };
// A broker step name to the row it belongs to (the default-branch check follows the first commit).
const STEP_ROW = { project: 'project', first_commit: 'first_commit', default_branch: 'first_commit', protect: 'protect', deploy_key: 'deploy_key', register: 'register' };

// stepStates: {row: 'done'|'failed'|'skipped'} for the seven rows.
// kind 'ok' (all done, the clone as given) or 'partial' (stopped at `step`).
export function stepStates(kind, step, cloneOk) {
  const out = {};
  if (kind === 'ok') {
    for (const k of STEP_ORDER) out[k] = 'done';
    out.clone = cloneOk ? 'done' : 'failed';
    return out;
  }
  const at = STEP_ORDER.indexOf(STEP_ROW[step]);
  for (let i = 0; i < STEP_ORDER.length; i++) {
    out[STEP_ORDER[i]] = i < at || (i === 0 && at >= 1) ? 'done' : i === at ? 'failed' : 'skipped';
  }
  return out;
}

// stepLabel: the fixed words of the row a broker step belongs to.
export function stepLabel(step, kind) {
  return has(STEP_ROW, step) ? (kind === 'github' ? STEP_LABEL_GITHUB : STEP_LABEL)[STEP_ROW[step]] : '알 수 없는 단계';
}

export function stepList(states, kind) {
  const labels = kind === 'github' ? STEP_LABEL_GITHUB : STEP_LABEL;
  return el('ol', { class: 'steps', 'aria-label': '만들기 단계' }, STEP_ORDER.map((k) => el('li', { 'data-step': k, 'data-status': states[k] }, [
    el('b', {}, labels[k]), ' — ', STATUS_LABEL[states[k]],
  ])));
}

// linkStepList: the two steps of a link or an attach (only these two can stop it half way).
export function linkStepList(step) {
  const rows = [['deploy_key', '배포 키 등록'], ['register', '이 서버에 등록']];
  const at = step === 'register' ? 1 : 0;
  return el('ol', { class: 'steps', 'aria-label': '연결 단계' }, rows.map(([k, label], i) => {
    const status = i < at ? 'done' : i === at ? 'failed' : 'skipped';
    return el('li', { 'data-step': k, 'data-status': status }, [el('b', {}, label), ' — ', STATUS_LABEL[status]]);
  }));
}

// hostName: how a provider kind is called on screen ("GitLab/GitHub" when the kind is not known).
export const hostName = (kind) => (kind === 'github' ? 'GitHub' : kind === 'gitlab' ? 'GitLab' : 'GitLab/GitHub');

// kindOfProvider: the kind of a provider id from a list of provider rows (undefined when unknown).
export const kindOfProvider = (list, id) => {
  const p = Array.isArray(list) ? list.find((x) => isObj(x) && x.id === id) : undefined;
  return p && PROVIDER_KINDS.includes(p.kind) ? p.kind : undefined;
};

// partialInfo: what to show of a create_partial or link_partial refusal. Every member of the
// server's `detail` is checked; if anything is off the page shows what it sent itself (the
// owner/group path and the name, or the remote path of a link) and says it cannot tell the step.
export function partialInfo(err, sent) {
  const own = sent.action === ACTION_CREATE_PROJECT ? sent.fields.group_path + '/' + sent.subject : sent.fields.remote_path;
  // a link or an attach never made the repository: its screen always says "linked", whatever origin the server names
  const origin = sent.action === ACTION_CREATE_PROJECT ? 'created' : 'linked';
  const fallback = { provider: sent.fields.provider, project: own, projectId: null, step: null, recorded: null, dropped: false, origin, valid: false };
  const d = err && err.detail;
  if (!isObj(d)) return fallback;
  const idOk = d.project_id === null || (Number.isSafeInteger(d.project_id) && d.project_id > 0);
  if (typeof d.provider !== 'string' || !PROVIDER_ID_RE.test(d.provider) || typeof d.project !== 'string' || !validRemotePath(d.project) ||
      !idOk || typeof d.step !== 'string' || !PARTIAL_STEPS.includes(d.step) || typeof d.orphan_recorded !== 'boolean' || typeof d.orphan_dropped !== 'boolean' ||
      (d.origin !== undefined && !ORPHAN_ORIGINS.includes(d.origin))) {
    return fallback;
  }
  return { provider: d.provider, project: d.project, projectId: d.project_id, step: d.step, recorded: d.orphan_recorded, dropped: d.orphan_dropped, origin, valid: true };
}

// orphanNotice: what to do with a half-made or half-linked repository, by where it came from.
// `created`: this tool made it, the person removes it. `linked`: it was there before; only the
// deploy key of this server may sit in it, and only that is removed.
export const ORPHAN_UNKNOWN_NOTICE = '출처 불명: 지우기 전에 확인하세요 — 이 도구는 제공자에서 아무것도 지우지 않습니다';
export function orphanNotice(origin, kind) {
  const host = hostName(kind);
  if (origin !== 'linked' && origin !== 'created') return ORPHAN_UNKNOWN_NOTICE;
  if (origin === 'linked') {
    return '원래 있던 저장소입니다. 지우지 마세요. 배포 키만 직접 제거하세요: ' + host + ' 저장소 설정의 배포 키(Deploy keys)에서 제목이 orchd로 시작하는 이 서버의 키를 지우면 됩니다. 이 도구는 이 저장소를 지우지 않습니다.';
  }
  return '이 도구가 만든 것: ' + host + '에서 직접 지우세요. 이 기록의 저장소는 등록되지 않아 이 도구가 삭제하지 않습니다(삭제 화면은 등록이 끝난 프로젝트만 지웁니다). 같은 이름으로 다시 만들려면 먼저 ' + host + '에서 그 ' + (kind === 'github' ? '저장소' : '프로젝트') + '를 정리해야 합니다.';
}
export const ORPHAN_NOTICE = orphanNotice('created', 'gitlab');

// orphanBlock shows one half-made project: the path and id as plain text.
export const NOT_LISTED_NOTICE = '이 목록에 남지 않았습니다. 아래 경로를 직접 기록해 두세요.';
export const DETAIL_INVALID_NOTICE = '서버가 알려 준 위치 정보를 확인할 수 없어(형식이 맞지 않음) 이 화면이 보낸 값으로 보여 드립니다. 실제 위치는 제공자 사이트에서 확인하세요.';

export function orphanBlock(info, kind) {
  const notListed = info.valid && (info.recorded === false || info.dropped === true);
  const host = hostName(kind);
  return el('div', { class: 'trunc', role: 'note', 'data-orphan': 'block' }, [
    notListed ? el('p', { 'data-orphan': 'not-listed' }, NOT_LISTED_NOTICE) : null,
    info.valid ? null : el('p', { 'data-orphan': 'invalid' }, DETAIL_INVALID_NOTICE),
    el('p', {}, [
      info.origin === 'linked' ? '이 저장소에 이 서버의 배포 키가 붙었을 수 있습니다(연결이 중간에 멈췄습니다): ' : '이 프로젝트가 ' + host + '에 만들어졌을 수 있습니다(일부만): ',
      el('span', { class: 'mono', 'data-orphan': 'project' }, info.project),
    ]),
    el('p', {}, [
      '프로젝트 ID: ',
      el('span', { class: 'mono', 'data-orphan': 'project_id' }, info.projectId !== null ? String(info.projectId) : info.valid ? '알 수 없음(' + host + '이 프로젝트 ID를 확인해 주지 않았습니다)' : '알 수 없음(서버가 알려 준 정보를 확인할 수 없었습니다)'),
    ]),
    el('p', { 'data-orphan': 'notice' }, orphanNotice(info.origin, kind)),
  ]);
}

// --- analysis of a proposal --------------------------------------------------

function mentions(sent, full, key) {
  const f = full;
  switch (sent.action) {
    case ACTION_SET_PROVIDER: return [sent.subject, f.base_url, f.ssh_url, f.token_ref];
    case ACTION_DELETE_PROVIDER: return [sent.subject];
    case ACTION_CREATE_GROUP: return [sent.subject, f.name, f.path, f.parent_path === '' ? '(최상위)' : f.parent_path];
    case ACTION_LINK_PROJECT: case ACTION_ATTACH_REMOTE: return [f.provider, f.remote_path, sent.subject, key ? key.fingerprint : ''];
    case ACTION_REMOVE_PROJECT: return [sent.subject];
    case ACTION_DELETE_REMOTE_REPO: return [f.remote_path, f.confirm_name];
    case ACTION_REMOVE_DEPLOY_KEY: return [sent.subject, f.remote_path];
    case ACTION_CLEAR_ORPHAN: return [sent.subject, f.project];
    default: return [f.provider, f.group_path, sent.subject, f.template, f.description === '' ? '(없음)' : f.description, key ? key.fingerprint : ''];
  }
}

// The actions whose proposal carries a deploy key (and whose change signs its hash).
const KEY_ACTIONS = [ACTION_CREATE_PROJECT, ACTION_LINK_PROJECT, ACTION_ATTACH_REMOTE];

// analyze checks the server's proposal against what the browser asked for.
// It returns {problems, full, key}: problems empty = the proposal says what the
// screen says. `full` are the exact fields to send with the apply (for a new
// project they carry the deploy key hash the BROWSER computed), `key` the
// deploy key as verified. `sent` is {action, subject, fields}: the page's own values.
async function analyze(sent, resp, expect) {
  const problems = [];
  if (!isObj(resp)) return { problems: ['shape'] };
  let full = sent.fields;
  let key = null;
  if (KEY_ACTIONS.includes(sent.action)) {
    const dk = resp.deploy_key;
    if (!isObj(dk) || typeof dk.public_key !== 'string' || dk.public_key.length > MAX_KEY_LINE || !DEPLOY_KEY_RE.test(dk.public_key) ||
        findHostile(dk.public_key) !== null || typeof dk.fingerprint !== 'string') {
      return { problems: ['deploy_key'] };
    }
    const hash = await deployKeyHash(dk.public_key);
    const fp = await sshFingerprint(dk.public_key);
    if (fp === null || fp !== dk.fingerprint) problems.push('fingerprint');
    full = { ...sent.fields, deploy_key_sha256: hash };
    key = { line: dk.public_key, fingerprint: fp, hash };
  }
  let canon;
  let change;
  try {
    canon = canonicalChange(sent.action, sent.subject, full);
    change = await computeChange(sent.action, sent.subject, full);
  } catch {
    return { problems: ['own_fields'] };
  }
  // the server echoes what was asked (and, for a project, the hash of the key it shows), nothing else
  let echoed = '';
  try {
    echoed = canonicalChange(resp.action, resp.subject, resp.fields);
  } catch {
    problems.push('echo');
  }
  if (echoed !== canon) problems.push('echo');
  // its hash is the browser's hash of the fields on screen
  if (resp.change !== change) problems.push('change');
  // the message to sign is canonical and says the same
  let msg = null;
  try {
    msg = checkRegistryMessage(resp.message);
  } catch {
    problems.push('message');
  }
  if (msg) {
    if (msg.action !== sent.action || msg.subject !== sent.subject || msg.change !== change) problems.push('message');
    if (resp.request !== msg.request || resp.issued !== msg.issued) problems.push('message');
  }
  // sentences: a bounded list of plain text that mentions every value the screen shows
  const pv = resp.preview;
  if (!Array.isArray(pv) || pv.length === 0 || pv.length > MAX_PREVIEW_LINES ||
      pv.some((x) => typeof x !== 'string' || x === '' || x.length > MAX_PREVIEW_CHARS || findHostile(x) !== null)) {
    problems.push('preview');
  } else {
    if (!mentions(sent, full, key).every((needle) => needle !== '' && pv.some((x) => x.includes(needle)))) problems.push('preview_values');
    const count = (line) => pv.filter((x) => x === line).length;
    const marks = (mark) => pv.filter((x) => x.includes(mark)).length;
    if (sent.action === ACTION_CREATE_PROJECT || sent.action === ACTION_LINK_PROJECT) {
      const on = count(PREVIEW_AI_ON);
      const off = count(PREVIEW_AI_OFF);
      if (full.ai_context ? !(on === 1 && off === 0) : !(off === 1 && on === 0)) problems.push('preview_ai');
    }
    if (KEY_ACTIONS.includes(sent.action)) {
      const w = count(PREVIEW_PUBLISH_WRITE);
      const r = count(PREVIEW_PUBLISH_READ);
      if (full.publish === 'write' ? !(w === 1 && r === 0) : !(r === 1 && w === 0)) problems.push('preview_publish');
    }
    // a GitHub create says, exactly once, whether the repository is public; a GitLab create never does
    if (sent.action === ACTION_CREATE_PROJECT) {
      const pub = count(PREVIEW_VIS_PUBLIC);
      const priv = count(PREVIEW_VIS_PRIVATE);
      const want = !has(full, 'visibility') ? [0, 0] : full.visibility === 'public' ? [1, 0] : [0, 1];
      if (pub !== want[0] || priv !== want[1]) problems.push('preview_visibility');
    }
    // W3f: a deletion proposal's preview is EXACTLY the lines this page rebuilt from its own values
    // (expect(resp) -> the lines, or null when the answer lacks what the lines need): nothing added, dropped or reordered.
    if (typeof expect === 'function') {
      const want = expect(resp);
      if (!Array.isArray(want) || want.length !== pv.length || want.some((x, i) => x !== pv[i])) problems.push('preview_exact');
    }
    // W3f: the fixed lines that say what a deletion does, exactly once or never (never the server's word for it)
    if (sent.action === ACTION_REMOVE_PROJECT) {
      if (marks(PREVIEW_DEL_LEGACY_MARK) !== (full.legacy_ack ? 1 : 0)) problems.push('preview_legacy');
      if (marks(PREVIEW_DEL_IRREVERSIBLE_MARK) !== 1) problems.push('preview_irreversible');
    }
    if (sent.action === ACTION_DELETE_REMOTE_REPO) {
      if (marks(PREVIEW_DEL_REMOTE_MARK) !== 1) problems.push('preview_remote');
      if (marks(PREVIEW_DEL_ALSO_MARK) !== (full.also_local ? 1 : 0)) problems.push('preview_also_local');
    }
    // a link or an attach adds this server's key to an existing repository: the server says so, once
    if (sent.action === ACTION_LINK_PROJECT || sent.action === ACTION_ATTACH_REMOTE) {
      if (count(PREVIEW_KEY_WARN) !== 1) problems.push('preview_key_warning');
    }
    // an attach with write permission says, exactly once, that local history may be pushed to the remote; never otherwise
    if (sent.action === ACTION_ATTACH_REMOTE) {
      if (count(PREVIEW_ATTACH_PUSH_WARN) !== (full.publish === 'write' ? 1 : 0)) problems.push('preview_attach_push');
    }
  }
  return { problems, full, key };
}

// --- the signed-change flow -----------------------------------------------------

// resolveSigner: the passkey of the logged-in principal (this tab's login, or
// GET /auth/signers once per page load).
async function resolveSigner(ctx) {
  const s = ctx.session;
  if (!s || s.kind !== 'session' || typeof s.principal !== 'string') return { error: TEXT.noPasskeySession };
  const rem = rememberedPasskey();
  if (rem && rem.signer === s.principal) return { id: rem.signer, credentialId: rem.credentialId, rpId: rem.rpId };
  const list = await activeSigners({ get: ctx.get });
  const found = list.find((x) => x.id === s.principal);
  if (!found) return { error: TEXT.noSigner };
  return { id: found.id, credentialId: found.credential_id };
}

const PROJECT_PATH = { [ACTION_CREATE_PROJECT]: 'create', [ACTION_LINK_PROJECT]: 'link', [ACTION_ATTACH_REMOTE]: 'attach' };
// W3f: the deletion endpoints (the orphan one lives under /orphans).
const DELETE_PATH = {
  [ACTION_REMOVE_PROJECT]: '/api/v1/projects/remove', [ACTION_DELETE_REMOTE_REPO]: '/api/v1/projects/delete-remote',
  [ACTION_REMOVE_DEPLOY_KEY]: '/api/v1/projects/remove-key', [ACTION_CLEAR_ORPHAN]: '/api/v1/orphans/clear',
};
const proposePath = (action) => (has(DELETE_PATH, action) ? DELETE_PATH[action] + '/propose' : has(PROJECT_PATH, action) ? '/api/v1/projects/' + PROJECT_PATH[action] + '/propose' : '/api/v1/providers/propose');
const applyPath = (action) => (has(DELETE_PATH, action) ? DELETE_PATH[action] + '/apply' : has(PROJECT_PATH, action) ? '/api/v1/projects/' + PROJECT_PATH[action] + '/apply' : '/api/v1/providers/apply');
// A link or an attach reads the registry through the broker while proposing: longer than a create's propose.
const proposeTimeout = (action) => (DELETE_ACTIONS.includes(action) ? DELETE_PROPOSE_TIMEOUT_MS : action === ACTION_LINK_PROJECT || action === ACTION_ATTACH_REMOTE ? LINK_PROPOSE_TIMEOUT_MS : PROPOSE_TIMEOUT_MS);
const applyTimeout = (action) => (DELETE_ACTIONS.includes(action) ? DELETE_APPLY_TIMEOUT_MS : action === ACTION_CREATE_PROJECT ? CREATE_APPLY_TIMEOUT_MS : action === ACTION_LINK_PROJECT ? LINK_APPLY_TIMEOUT_MS
  : action === ACTION_ATTACH_REMOTE ? ATTACH_APPLY_TIMEOUT_MS : PROVIDER_TIMEOUT_MS);

// makeFlow runs ONE signed change at a time for a view: propose -> check ->
// passkey -> apply. The view paints from `flow` (phase, sent, resp, problems,
// signer, note, result) and is told by cfg.onChange() after every step, and by
// cfg.onResult(result) once the apply has ended (the view then re-reads its lists). cfg.prepare(result, sent),
// when given, runs synchronously before the first paint of the result.
//   phase: idle | proposing | confirm | sending | done
//   result: {ok: true, data} | {ok: false, err, unknown}
// A proposal is spent when the signature is made: whatever the answer, it is
// never signed or sent again.
export function makeFlow(ctx, cfg) {
  const f = {
    phase: 'idle', sent: null, resp: null, full: null, key: null, problems: [], signer: null, signerNote: null, note: null, result: null, busy: false,
  };
  let propSeq = 0;
  let signerP = null;
  const gone = () => !!(ctx.signal && ctx.signal.aborted);
  const changed = () => { if (!gone()) cfg.onChange(); };

  f.reset = () => {
    propSeq++; // an answer still in flight is for an abandoned change
    Object.assign(f, { phase: 'idle', sent: null, resp: null, full: null, key: null, problems: [], signer: null, signerNote: null, note: null, result: null, busy: false });
  };

  async function resolveSignerOnce(sent) {
    if (!supported()) {
      f.signerNote = waText('unsupported');
    } else {
      try {
        if (!signerP) signerP = resolveSigner(ctx);
        const s = await signerP;
        if (s.error) {
          signerP = null;
          f.signerNote = s.error;
        } else {
          f.signer = s;
          f.signerNote = '서명에 쓸 패스키: ' + inline(s.id, 64);
        }
      } catch {
        signerP = null;
        f.signerNote = TEXT.signersFailed;
      }
    }
  }

  // propose: false when a proposal is already out (a double click); the view
  // shows f.note (a fixed text) when the phase is back to idle.
  f.propose = async (sent, expect) => {
    if (f.busy) return;
    f.busy = true;
    f.note = null;
    const my = ++propSeq;
    f.phase = 'proposing';
    changed();
    let resp;
    try {
      resp = await ctx.post(proposePath(sent.action), sent, { noRetry429: true, timeoutMs: proposeTimeout(sent.action) });
    } catch (err) {
      if (gone() || my !== propSeq) return;
      f.busy = false;
      f.phase = 'idle';
      const code = err && typeof err.code === 'string' ? err.code : '';
      const kind = cfg.kindOf ? cfg.kindOf(sent) : undefined;
      const custom = cfg.failText ? cfg.failText(err, sent) : null;
      f.note = { error: true, text: custom || (err && err.status === 0 ? TEXT.connect : has(ERROR_TEXT, code) ? failureText(err, kind) : TEXT.proposeFailed) };
      changed();
      return;
    }
    if (gone() || my !== propSeq) return;
    const a = await analyze(sent, resp, expect);
    if (gone() || my !== propSeq) return;
    f.problems = a.problems;
    f.full = a.full || null;
    f.key = a.key || null;
    f.sent = sent;
    f.resp = resp;
    f.phase = 'confirm';
    f.signer = null;
    f.signerNote = null;
    if (f.problems.length > 0) {
      f.busy = false;
      changed();
      return;
    }
    // busy stays set until the signer is resolved too: no second proposal in between
    changed();
    await resolveSignerOnce(sent);
    if (gone() || my !== propSeq) return;
    f.busy = false;
    changed();
  };

  f.canSign = () => f.phase === 'confirm' && !f.busy && f.problems.length === 0 && !!f.signer && !!f.resp && supported();

  f.sign = async () => {
    if (!f.canSign()) return; // also a double click: busy is set below, before any await
    const cur = { sent: f.sent, resp: f.resp, full: f.full };
    f.busy = true;
    f.note = { error: false, text: TEXT.waiting };
    changed();
    let signature;
    try {
      signature = await signRegistryChange(cur.resp.message, f.signer);
    } catch (err) {
      if (gone() || f.resp !== cur.resp) return;
      f.busy = false;
      f.note = { error: true, text: err && err.name === 'WebAuthnError' ? waText(err.code) : waText('authenticator_failed') };
      changed();
      return;
    }
    // The user left while the passkey prompt was open: drop the signature, send nothing.
    if (gone() || f.resp !== cur.resp) return;
    const body = {
      action: cur.sent.action, subject: cur.sent.subject, fields: cur.full,
      change: cur.resp.change, request: cur.resp.request, issued: cur.resp.issued, signature,
    };
    // From here the proposal is spent.
    f.phase = 'sending';
    f.note = null;
    f.result = null;
    changed();
    const timeoutMs = applyTimeout(cur.sent.action);
    let data;
    try {
      data = await ctx.post(applyPath(cur.sent.action), body, { noRetry429: true, timeoutMs });
    } catch (err) {
      if (gone()) return;
      f.busy = false;
      f.phase = 'done';
      f.result = { ok: false, err, unknown: isUnknown(err) };
      if (cfg.prepare) cfg.prepare(f.result, cur.sent);
      changed();
      await cfg.onResult(f.result, cur.sent);
      return;
    }
    if (gone()) return;
    f.busy = false;
    f.phase = 'done';
    f.result = { ok: true, data };
    if (cfg.prepare) cfg.prepare(f.result, cur.sent); // what the result screen needs is ready BEFORE it is drawn
    changed();
    await cfg.onResult(f.result, cur.sent);
  };
  return f;
}

// --- shared rendering --------------------------------------------------------

export const li = (k, v, attrs) => el('li', attrs || {}, [el('b', {}, k), ': ', el('span', { class: 'mono' }, v)]);

export function noteNode(note) {
  return note ? el('p', { class: note.error ? ['note', 'err'] : ['note', 'ok'], role: note.error ? 'alert' : 'status' }, note.text) : null;
}

// confirmPanel draws the confirmation screen of a flow in the `confirm` phase.
// opts: {title, lead, facts: [li...], extra: [nodes], warn: node|null, onSign, onCancel}
export function confirmPanel(flow, opts) {
  let controls;
  if (flow.problems.length > 0) {
    controls = el('div', { class: 'trunc', role: 'alert', 'data-warn': 'mismatch' }, TEXT.mismatch);
  } else {
    controls = el('div', { class: 'confirm' }, [
      flow.signerNote ? el('p', { class: 'note' }, flow.signerNote) : null,
      flow.signer && supported()
        ? el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'sign', disabled: flow.busy, on: { click: opts.onSign } }, '패스키로 서명하고 적용')
        : null,
    ]);
  }
  const msgText = flow.problems.length === 0 ? flow.resp.message : '';
  return el('div', { class: ['panel', 'canvas'] }, [
    el('h1', { id: 'h-confirm' }, opts.title),
    el('p', { class: 'lead' }, opts.lead),
    opts.warn || null,
    el('ul', { class: 'pfacts', 'aria-label': '바뀌는 내용' }, opts.facts),
    ...(opts.extra || []),
    flow.problems.length === 0
      ? el('section', { class: 'sec', 'aria-labelledby': 'h-says' }, [
        el('h2', { id: 'h-says' }, '서버의 설명'),
        el('ul', {}, flow.resp.preview.map((x) => el('li', { 'data-preview': 'item' }, x))),
      ])
      : null,
    msgText
      ? el('section', { class: 'sec', 'aria-labelledby': 'h-msg' }, [
        el('h2', { id: 'h-msg' }, '서명할 메시지 (서버가 만든 원문)'),
        el('pre', { class: 'msg', role: 'region', tabindex: '0', 'aria-label': '서명할 메시지' }, msgText),
        el('p', { class: 'note' }, '이 화면은 위 내용으로 변경의 해시를 직접 다시 계산해 서버가 보낸 값과 같을 때만 서명 단추를 보여 줍니다. 5분 안에 서명해야 합니다.'),
      ])
      : null,
    controls,
    noteNode(flow.note),
    el('p', {}, [el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: opts.onCancel } }, '취소')]),
  ]);
}

export { TEXT as FLOW_TEXT };

export const EDIT_WARNING = '제공자를 바꾸면 이 제공자를 쓰는 프로젝트의 만들기·복제·게시가 모두 바뀐 설정으로 이어집니다. 토큰을 바꾸면 새 토큰의 권한으로 제공자(GitLab/GitHub)에 접근합니다. ' +
  '이 제공자를 쓰는 프로젝트가 있으면 API 주소와 ssh 주소는 바꿀 수 없습니다(서버가 거부합니다). 서명하면 바로 적용됩니다.';

// --- the providers screen ----------------------------------------------------

const ok = (b) => (b ? '예' : '아니오');

function providerRows(r) {
  return r && Array.isArray(r.providers) ? r.providers.filter((p) => isObj(p)).slice(0, MAX_ROWS) : [];
}

// A provider this page may act on: every value has the shape the server enforces.
const actionable = (p) => typeof p.id === 'string' && PROVIDER_ID_RE.test(p.id) && PROVIDER_KINDS.includes(p.kind) && validBaseUrl(p.base_url) && validSshUrl(p.ssh_url) &&
  typeof p.token_ref === 'string' && TOKEN_REF_RE.test(p.token_ref);

// usableTokensOfKind: the only tokens a provider of that kind may point at (usable ones of the
// SAME kind; never a value, only names).
export function usableTokensOfKind(list, kind) {
  if (!isObj(list) || !Array.isArray(list.tokens)) return [];
  return list.tokens.filter((t) => isObj(t) && t.kind === kind && t.usable === true && typeof t.ref === 'string' && TOKEN_REF_RE.test(t.ref)).slice(0, MAX_ROWS);
}
export const usableGitlabTokens = (list) => usableTokensOfKind(list, 'gitlab');

export function providersView(ctx) {
  ctx.setCrumb('제공자');
  const st = {
    step: 'loading', // loading | list | form | confirm | result
    list: null,
    listFailed: null,
    tokens: null, // the token list as read, or null (the token columns say so)
    note: null,
    form: null,
    edit: null, // the provider row an edit started from
    view: null,
    result: null,
  };
  let listSeq = 0;
  const gone = () => !!(ctx.signal && ctx.signal.aborted);
  const flow = makeFlow(ctx, {
    onChange: () => {
      if (st.step === 'confirm' && (flow.phase === 'sending' || flow.phase === 'done')) st.step = 'result';
      if (st.step === 'confirm' || st.step === 'result' || st.step === 'form') paint();
    },
    onResult,
    kindOf: (sent) => (sent.action === ACTION_SET_PROVIDER ? sent.fields.kind : st.view && st.view.row ? st.view.row.kind : undefined),
  });

  function paint() {
    replaceContent(ctx.root, render());
  }

  async function loadAll() {
    const my = ++listSeq;
    const [pr, tk] = await Promise.allSettled([ctx.get('/api/v1/providers'), ctx.get('/api/v1/settings/tokens')]);
    if (gone() || my !== listSeq) return;
    if (pr.status === 'fulfilled') {
      st.list = providerRows(pr.value);
      st.listFailed = null;
    } else {
      st.listFailed = pr.reason;
    }
    st.tokens = tk.status === 'fulfilled' && isObj(tk.value) && Array.isArray(tk.value.tokens) ? tk.value : null;
    if (st.step === 'loading' || st.step === 'list') {
      st.step = 'list';
      paint();
    }
  }

  async function onResult(result) {
    // Whatever the answer, the list is read again: a refusal may mean it changed meanwhile, an unknown outcome certainly does.
    await loadAll();
  }

  function toList() {
    flow.reset();
    st.step = 'list';
    st.note = null;
    st.form = null;
    st.edit = null;
    st.view = null;
    paint();
  }

  async function backToList() {
    toList();
    st.step = 'loading';
    paint();
    await loadAll();
  }

  // ---- form ----
  function tokenInfo(ref, kind) {
    if (st.tokens === null) return null;
    return st.tokens.tokens.find((t) => isObj(t) && t.ref === ref && t.kind === kind) || false;
  }

  // fillTokens: the token choices of the chosen kind (usable ones only), keeping a choice that is still valid.
  function fillTokens(f, keep) {
    const toks = usableTokensOfKind(st.tokens, f.kind.value);
    clear(f.token);
    for (const t of toks) {
      f.token.appendChild(el('option', { value: t.ref }, t.ref + (typeof t.expires === 'string' && t.expires !== '' ? ' (만료 ' + inline(t.expires, 12) + ')' : '')));
    }
    f.token.value = keep && toks.some((t) => t.ref === keep) ? keep : (toks[0] ? toks[0].ref : '');
    f.noTokens = toks.length === 0;
  }

  // The kind select of a NEW provider: the address defaults of that kind are filled in when the
  // address fields are empty or still hold the other kind's default (a typed address is never replaced).
  function onKind() {
    const f = st.form;
    const gh = f.kind.value === 'github';
    const baseDefault = (v) => v === '' || v === GITHUB_API_URL;
    const sshDefault = (v) => v === '' || v === GITHUB_SSH_URL;
    if (baseDefault(f.base.value.trim())) f.base.value = gh ? GITHUB_API_URL : '';
    if (sshDefault(f.ssh.value.trim())) f.ssh.value = gh ? GITHUB_SSH_URL : '';
    fillTokens(f, null);
    paint();
  }

  function openForm(row) {
    const f = {};
    f.kind = el('select', { id: 'pv-kind', 'data-focus-key': 'kind', disabled: !!row, on: { change: onKind } },
      PROVIDER_KINDS.map((k) => el('option', { value: k }, hostName(k))));
    f.kind.value = row ? row.kind : 'gitlab';
    f.id = el('input', { type: 'text', id: 'pv-id', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: '40', readonly: !!row, 'data-focus-key': 'id' });
    f.id.value = row ? row.id : '';
    f.base = el('input', { type: 'text', id: 'pv-base', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(URL_MAX), 'data-focus-key': 'base' });
    f.base.value = row ? row.base_url : '';
    f.ssh = el('input', { type: 'text', id: 'pv-ssh', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(URL_MAX), 'data-focus-key': 'ssh' });
    f.ssh.value = row ? row.ssh_url : '';
    f.token = el('select', { id: 'pv-token', 'data-focus-key': 'token' });
    fillTokens(f, row ? row.token_ref : null);
    f.problem = el('p', { class: ['note', 'err'], role: 'alert', 'data-problem': 'form' });
    f.problem.hidden = true;
    st.form = f;
    st.edit = row || null;
    st.step = 'form';
    st.note = null;
    paint();
  }

  function formProblem(text) {
    const f = st.form;
    clear(f.problem);
    if (text) f.problem.append(text);
    f.problem.hidden = !text;
  }

  function submitForm() {
    const f = st.form;
    if (!f || flow.busy) return;
    const id = f.id.value.trim();
    const base = f.base.value.trim();
    const ssh = f.ssh.value.trim();
    const tokenRef = f.token.value;
    const kind = f.kind.value;
    if (!PROVIDER_KINDS.includes(kind)) return formProblem('제공자 종류를 목록에서 고르세요.');
    const gh = kind === 'github';
    if (!PROVIDER_ID_RE.test(id)) return formProblem('제공자 id는 소문자로 시작하고 소문자·숫자·하이픈만 써서 40자 이내로 적으세요.');
    if (!st.edit && st.list && st.list.some((p) => p.id === id)) return formProblem('같은 id의 제공자가 이미 있습니다. 목록의 "수정"을 쓰세요.');
    if (gh) {
      const why = githubBaseUrlProblem(base);
      if (why === 'web_host') return formProblem('github.com은 웹 주소입니다. GitHub API 주소는 https://api.github.com 입니다(GitHub Enterprise Server는 그 서버의 주소).');
      if (why === 'http') return formProblem('GitHub API 주소는 https:// 로 시작해야 합니다(http는 사설 주소의 GitHub Enterprise Server에만 쓸 수 있습니다).');
      if (why !== null) return formProblem('GitHub API 주소는 https://호스트[:포트] 형태여야 합니다(경로·계정·쿼리 없음). 예: https://api.github.com');
    } else if (!validBaseUrl(base)) {
      return formProblem('GitLab 주소는 http:// 또는 https://로 시작하는 주소여야 합니다(호스트와 선택적 포트만, 경로·계정·쿼리 없음). 예: http://gitlab.example:18082');
    }
    if (!validSshUrl(ssh)) return formProblem('ssh 주소는 ssh://git@호스트 또는 ssh://git@호스트:포트 형태여야 합니다(경로 없음).');
    if (st.tokens === null) return formProblem('토큰 목록을 읽지 못해 토큰을 고를 수 없습니다. 목록으로 돌아가 "목록 새로고침"으로 다시 읽은 뒤 시도하세요.');
    if (!usableTokensOfKind(st.tokens, kind).some((t) => t.ref === tokenRef)) {
      return formProblem('토큰을 목록에서 고르세요. 쓸 수 있는 ' + kind + ' 종류 토큰만 나옵니다. 없으면 토큰 화면에서 먼저 등록하세요.');
    }
    if (st.edit) {
      if (st.edit.in_use === true && (base !== st.edit.base_url || ssh !== st.edit.ssh_url)) {
        return formProblem('이 제공자를 쓰는 프로젝트가 있어(사용 중) ' + (gh ? 'API' : 'GitLab') + ' 주소와 ssh 주소는 바꿀 수 없습니다. 토큰만 바꿀 수 있습니다.');
      }
      if (base === st.edit.base_url && ssh === st.edit.ssh_url && tokenRef === st.edit.token_ref) return formProblem('바뀐 값이 없습니다.');
    }
    formProblem('');
    start({ action: ACTION_SET_PROVIDER, subject: id, fields: { kind, base_url: base, ssh_url: ssh, token_ref: tokenRef } }, st.edit ? { old: st.edit } : null);
  }

  async function start(sent, view) {
    if (flow.busy) return;
    st.view = view;
    await flow.propose(sent);
    if (flow.phase === 'idle' && flow.note) {
      // refused before anything was signed: said where the person is
      if (st.step === 'form') formProblem(flow.note.text);
      else st.note = flow.note;
      flow.note = null;
      paint();
      return;
    }
    if (flow.phase === 'confirm') {
      st.step = 'confirm';
      paint();
    }
  }

  function startDelete(row) {
    if (flow.busy || !actionable(row) || row.in_use === true) return;
    start({ action: ACTION_DELETE_PROVIDER, subject: row.id, fields: {} }, { row });
  }

  // ---- rendering ----
  function render() {
    switch (st.step) {
      case 'form':
        return renderForm();
      case 'confirm':
        return renderConfirm();
      case 'result':
        return renderResult();
      case 'list':
        return renderList();
      default:
        return el('div', { class: ['panel', 'canvas'] }, [el('p', { class: 'lead' }, '불러오는 중…')]);
    }
  }

  function providerRow(p) {
    const can = actionable(p);
    const inUse = p.in_use === true;
    const t = tokenInfo(p.token_ref, p.kind);
    const last4 = t ? inline(t.last4, 8) : t === null ? '-' : '목록에 없음';
    const expires = t ? (typeof t.expires === 'string' && t.expires !== '' ? inline(t.expires, 12) : '없음') : '-';
    let actions;
    if (can) {
      actions = [
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'edit-' + p.id, on: { click: () => openForm(p) } }, ['수정', el('span', { class: 'sr' }, ' (' + inline(p.id, 64) + ')')]),
        ' ',
        el('button', {
          type: 'button', class: 'btn', 'data-focus-key': 'del-' + p.id, disabled: inUse, on: { click: () => startDelete(p) },
        }, ['삭제', el('span', { class: 'sr' }, ' (' + inline(p.id, 64) + ')')]),
        inUse ? el('div', { class: 'note' }, '사용 중이라 삭제할 수 없습니다') : null,
      ];
    } else {
      actions = el('span', { class: 'note' }, '-');
    }
    return el('tr', {}, [
      el('td', { class: 'mono', 'data-label': 'ID' }, inline(p.id, 64)),
      el('td', { 'data-label': '종류' }, PROVIDER_KINDS.includes(p.kind) ? hostName(p.kind) : inline(p.kind, 32)),
      el('td', { class: 'mono', 'data-label': 'API 주소' }, inline(p.base_url, 200)),
      el('td', { class: 'mono', 'data-label': 'ssh 주소' }, inline(p.ssh_url, 200)),
      el('td', { 'data-label': '토큰' }, [
        el('div', { class: 'mono' }, inline(p.token_ref, 64)),
        el('small', { class: 'note' }, '끝 4자 ' + last4 + ' · 만료 ' + expires),
      ]),
      el('td', { 'data-label': '사용 중' }, ok(inUse)),
      el('td', {}, actions),
    ]);
  }

  function renderList() {
    if (st.list === null) {
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { 'data-focus-key': 'err' }, '불러오지 못했습니다'),
        el('p', { class: 'lead', role: 'alert' }, failureText(st.listFailed)),
        el('p', {}, [el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reload', on: { click: () => loadAll() } }, '다시 시도')]),
      ]);
    }
    const rows = st.list.map(providerRow);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-providers' }, '제공자'),
      el('p', { class: 'lead' }, st.list.length === 0 ? '등록된 제공자가 없습니다.'
        : '등록된 제공자(GitLab·GitHub) ' + st.list.length + '개. 제공자를 추가하거나 바꾸려면 패스키 서명이 필요합니다. 이 화면에는 토큰 값을 넣는 곳이 없습니다(토큰은 이름으로 고릅니다).'),
      el('div', { class: 'row' }, [
        el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'add', disabled: flow.busy, on: { click: () => openForm(null) } }, '제공자 추가'),
        ' ',
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reload', disabled: flow.busy, on: { click: () => { st.note = null; loadAll(); } } }, '목록 새로고침'),
      ]),
      noteNode(st.note),
      st.list.length > 0
        ? el('div', { class: 'card' }, [el('div', { class: 'tscroll' }, [el('table', { class: 'ptable' }, [
          el('caption', { class: 'sr' }, '등록된 제공자'),
          el('thead', {}, [el('tr', {}, ['ID', '종류', 'API 주소', 'ssh 주소', '토큰', '사용 중', '변경'].map((h) => el('th', { scope: 'col' }, h)))]),
          el('tbody', {}, rows),
        ])])])
        : null,
      el('p', { class: 'note' }, '도달 여부(연결이 되는지)는 이 화면에서 확인하지 않습니다. 토큰은 토큰 화면에서 등록합니다.'),
      el('p', {}, [el('a', { href: '#/tokens' }, '토큰 화면')]),
    ]);
  }

  function renderForm() {
    const f = st.form;
    const gh = f.kind.value === 'github';
    const field = (labelText, id, control, hint) => el('div', { class: 'field' }, [el('label', { for: id }, labelText), control, hint ? el('span', { class: 'hint' }, hint) : null]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-providers' }, st.edit ? '제공자 수정' : '제공자 추가'),
      el('p', { class: 'lead' }, 'GitLab 서버 또는 GitHub을 제공자로 등록합니다. 다음 화면에서 바뀔 내용을 모두 확인한 뒤 패스키로 서명합니다.'),
      st.edit ? el('div', { class: 'trunc', role: 'note', 'data-warn': 'edit' }, EDIT_WARNING) : null,
      el('div', { class: 'form' }, [
        field('제공자 종류', 'pv-kind', f.kind, st.edit ? '이미 있는 제공자의 종류는 바꿀 수 없습니다(지우고 다시 추가하세요).' : 'GitHub을 고르면 주소 기본값이 채워집니다. 직접 고칠 수 있습니다.'),
        field('제공자 id', 'pv-id', f.id, st.edit ? '이미 있는 제공자의 id는 바꿀 수 없습니다.' : '소문자·숫자·하이픈, 40자 이내'),
        field(gh ? 'GitHub API 주소' : 'GitLab 주소', 'pv-base', f.base, gh ? 'https://api.github.com — GitHub Enterprise Server는 그 서버의 https 주소. github.com(웹 주소)은 쓸 수 없습니다.'
          : 'http(s)://호스트[:포트] — 경로·계정·쿼리 없음. 서버가 루프백·링크 로컬 주소를 거부합니다.'),
        field('ssh 주소', 'pv-ssh', f.ssh, gh ? 'ssh://git@github.com — 복제와 게시에 쓰입니다(github.com의 호스트 키는 서버 관리자가 미리 등록해야 합니다).' : 'ssh://git@호스트[:포트] — 복제와 게시에 쓰입니다.'),
        field('사용할 토큰(이름)', 'pv-token', f.token, st.tokens === null ? '토큰 목록을 읽지 못했습니다. 목록으로 돌아가 "목록 새로고침"으로 다시 읽으세요.' : f.noTokens ? '쓸 수 있는 ' + f.kind.value + ' 종류 토큰이 없습니다. 토큰 화면에서 먼저 등록하세요.'
          : gh ? 'GitHub에 저장소를 만들고 배포 키를 등록할 권한이 있는 토큰. 이름만 고릅니다.' : 'GitLab에 그룹·프로젝트를 만들 권한이 있는 관리자 토큰. 이름만 고릅니다.'),
        f.problem,
        noteNode(st.note),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm', disabled: flow.busy, on: { click: submitForm } }, '변경 내용 확인'),
          ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: toList } }, '취소'),
        ]),
      ]),
    ]);
  }

  function renderConfirm() {
    const s = flow.sent;
    let factsList;
    if (s.action === ACTION_SET_PROVIDER) {
      const old = st.view && st.view.old;
      const change = (k, oldV, newV) => (old && oldV !== newV ? oldV + ' → ' + newV : newV);
      factsList = [
        li('제공자 id', s.subject, { 'data-field': 'id' }),
        li('종류', s.fields.kind, { 'data-field': 'kind' }),
        li(s.fields.kind === 'github' ? 'GitHub API 주소' : 'GitLab 주소', change('base_url', old && old.base_url, s.fields.base_url), { 'data-field': 'base_url' }),
        li('ssh 주소', change('ssh_url', old && old.ssh_url, s.fields.ssh_url), { 'data-field': 'ssh_url' }),
        li('사용할 토큰', change('token_ref', old && old.token_ref, s.fields.token_ref), { 'data-field': 'token_ref' }),
      ];
    } else {
      const row = (st.view && st.view.row) || {};
      factsList = [
        li('삭제할 제공자 id', s.subject, { 'data-field': 'id' }),
        li((row.kind === 'github' ? 'GitHub API' : 'GitLab') + ' 주소(목록 기준)', inline(row.base_url, 200), { 'data-field': 'base_url' }),
        li('ssh 주소(목록 기준)', inline(row.ssh_url, 200), { 'data-field': 'ssh_url' }),
      ];
    }
    return confirmPanel(flow, {
      title: '변경 내용 확인',
      lead: '아래 내용이 전부입니다. 서명하면 바로 적용됩니다.',
      facts: factsList,
      warn: s.action === ACTION_SET_PROVIDER && st.edit ? el('div', { class: 'trunc', role: 'note', 'data-warn': 'edit' }, EDIT_WARNING) : null,
      onSign: () => flow.sign(),
      onCancel: toList,
    });
  }

  function renderResult() {
    const r = flow.result;
    const s = flow.sent;
    let title;
    let body;
    let kind;
    if (r === null) {
      title = '적용하는 중';
      body = TEXT.sending;
      kind = 'pending';
    } else if (r.ok) {
      title = '완료';
      body = s.action === ACTION_DELETE_PROVIDER ? '제공자를 삭제했습니다.' : '제공자를 저장했습니다.';
      kind = 'ok';
    } else {
      title = '변경하지 못했습니다';
      const pk = s.action === ACTION_SET_PROVIDER ? s.fields.kind : st.view && st.view.row ? st.view.row.kind : undefined;
      body = r.unknown ? failureText({ code: 'outcome_unknown' }, pk) : failureText(r.err, pk);
      kind = 'error';
    }
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-providers' }, title),
      el('p', { class: kind === 'error' ? ['note', 'err'] : ['note', 'ok'], role: kind === 'error' ? 'alert' : 'status', 'data-result': kind }, body),
      kind === 'pending' ? null : el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
    ]);
  }

  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  return loadAll();
}

registerView('providers', providersView);
