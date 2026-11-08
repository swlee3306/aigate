import { useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { Badge, Bar, Caret, Collapse, Count, Prompt, Stage, Window, type Act } from '../../kit'
import './orch.css'

// The orch console, dark. A new approval opens at the top of the list and
// waits for a person. They approve it in a terminal (orch approve 3, then yes),
// and the row's pill turns to Approved on the same pill. After a 2s hold the
// terminal leaves, the row closes the way it opened and the story resets while
// it is closed.
//
// Real strings: the row (what, which task, its target), its two statuses, the
// command and its short output. The rest is placeholder at real line heights.

type S = {
  open: boolean
  approved: boolean
  term: boolean
  typed: string
  out: number
  yes: string
}

const initial: S = { open: false, approved: false, term: false, typed: '', out: 0, yes: '' }
const POSTER: S = { open: true, approved: true, term: true, typed: 'orch approve 3', out: 3, yes: 'yes' }

async function script(a: Act<S>) {
  await a.wait(900)
  a.mark('arrive')
  a.put({ open: true })
  await a.wait(1700)
  a.mark('terminal')
  a.put({ term: true })
  await a.wait(1000)
  await a.type('typed', 'orch approve 3')
  await a.wait(350)
  a.put({ out: 1 })
  await a.wait(900)
  a.put({ out: 2 })
  await a.wait(700)
  await a.type('yes', 'yes')
  await a.wait(350)
  a.put({ out: 3 })
  await a.wait(550)
  a.mark('approved')
  a.put({ approved: true })
  await a.wait(2300)
  // The terminal leaves the way it came, then the row closes; the story's
  // state goes back while nobody can see it
  a.put({ term: false })
  await a.wait(600)
  a.put({ open: false })
  await a.wait(800)
  await a.snap({ approved: false, typed: '', yes: '', out: 0 })
}

const OLD = [
  { msg: '보호 파일 수정', tag: '작업 #2', meta: 'tests/**', when: '2시간 전', id: '항목 1' },
  { msg: '정책 변경 게시', tag: '작업 #2', meta: 'push_branch', when: '2시간 전', id: '항목 2' },
]

export default function OrchApprove() {
  return (
    <Stage<S> width={760} height={540} theme="dark" initial={initial} poster={POSTER} script={script} className="oa">
      {(s) => (
        <>
          <Window title="승인" className="oa-win" sidebarWidth={168} sidebar={<Side pending={s.open && !s.approved ? 1 : 0} />}>
            <div className="oa-body">
              <div className="oa-list">
                <Collapse open={s.open} reveal="drop">
                  <Row
                    msg="정책 변경 게시"
                    tag="작업 #5"
                    meta="push_branch"
                    when="방금"
                    id="항목 3"
                    status={
                      <Badge tone={s.approved ? 'success' : 'warning'} dot className="oa-badge">
                        <Label approved={s.approved} />
                      </Badge>
                    }
                  />
                </Collapse>
                {OLD.map((r) => (
                  <Row key={r.id} {...r} status={<Badge tone="neutral" dot className="oa-badge oa-badge--old">승인됨</Badge>} />
                ))}
                {[0, 1, 2, 3, 4].map((i) => (
                  <div key={i} className="oa-row oa-row--filler">
                    <span className="oa-lines">
                      <Bar w={[170, 140, 190, 120, 160][i]!} h={8} r={4} tone={2} />
                      <Bar w={100} h={7} r={3.5} tone={1} />
                    </span>
                    <Bar w={60} h={18} r={9} tone={1} />
                    <Bar w={34} h={7} r={3.5} tone={1} />
                  </div>
                ))}
              </div>
            </div>
          </Window>

          <Window chrome="terminal" lights="grey" title="orch-vm — zsh" className="oa-term" data-show={s.term || undefined}>
            <div className="ss-term oa-term__body">
              <Prompt path="~" data-typing data-snap>
                {s.typed}
                {s.out === 0 && <Caret />}
              </Prompt>
              <div className="oa-out" data-on={s.out >= 1 || undefined}>
                <div>작업 #5 게시 행위 승인</div>
                <div className="dim">행위 push_branch · 로컬 저장소</div>
              </div>
              <div className="oa-out" data-typing data-snap data-on={s.out >= 2 || undefined}>
                <span className="dim">Type 'yes' to approve:</span> {s.yes}
                {s.out === 2 && <Caret />}
              </div>
              <div className="oa-out ok" data-on={s.out >= 3 || undefined}>approved.</div>
            </div>
          </Window>
        </>
      )}
    </Stage>
  )
}

function Side({ pending }: { pending: number }) {
  return (
    <nav className="oa-side">
      <span className="oa-nav">
        <Bar w={12} h={12} r={3} tone={2} />
        개요
      </span>
      <span className="oa-nav">
        <Bar w={12} h={12} r={3} tone={2} />
        작업
      </span>
      <span className="oa-nav" data-active>
        <Bar w={12} h={12} r={3} tone={4} />
        승인
        <span className="oa-count" data-on={pending > 0 || undefined}>
          <Count value={pending} duration={700} />
        </span>
      </span>
      {[76, 52].map((w, i) => (
        <span key={i} className="oa-nav">
          <Bar w={12} h={12} r={3} tone={2} />
          <Bar w={w} h={6} tone={2} />
        </span>
      ))}
    </nav>
  )
}

// The status word. The pill keeps its left edge and only its right edge
// moves: the cell eases to the width of the word that is showing.
function Label({ approved }: { approved: boolean }) {
  const words = useRef<HTMLSpanElement[]>([])
  const [w, setW] = useState<number[]>([])
  useLayoutEffect(() => setW(words.current.map((e) => e.offsetWidth)), [])
  const on = approved ? 1 : 0
  const shrink = w.length === 2 && w[on]! < w[1 - on]!
  return (
    <span className="oa-label" data-shrink={shrink || undefined} style={w.length ? { width: w[on] } : undefined}>
      {['승인 대기', '승인됨'].map((t, i) => (
        <span key={t} ref={(e) => { if (e) words.current[i] = e }} className="oa-word" data-on={i === on || undefined}>{t}</span>
      ))}
    </span>
  )
}

function Row({ msg, tag, meta, when, id, status }: { msg: string; tag: string; meta: string; when: string; id: string; status: ReactNode }) {
  return (
    <div className="oa-row">
      <span className="oa-lines">
        <span className="oa-msg">
          {msg}
          <span className="oa-tag">{tag}</span>
        </span>
        <span className="oa-meta">
          <span className="oa-mono">{meta}</span>
          <span className="oa-when">{when}</span>
        </span>
      </span>
      {status}
      <span className="oa-id">{id}</span>
    </div>
  )
}
