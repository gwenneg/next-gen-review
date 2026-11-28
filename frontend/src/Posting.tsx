import { useEffect, useState, type FormEvent } from 'react'
import {
  addComment,
  deleteComment,
  getPosting,
  postReview,
  setComment,
  setMainComment,
  type Posting,
  type Review,
  type ReviewComment,
  type ReviewEvent,
  type Side,
} from './api'

const verdicts: { value: ReviewEvent; label: string }[] = [
  { value: 'COMMENT', label: 'Comment' },
  { value: 'APPROVE', label: 'Approve' },
  { value: 'REQUEST_CHANGES', label: 'Request changes' },
]
const verdictText = (e: string) => verdicts.find((v) => v.value === e)?.label ?? e

const where = (c: Pick<ReviewComment, 'path' | 'line' | 'startLine' | 'side'>) =>
  `${c.path}:${c.startLine && c.startLine !== c.line ? `${c.startLine}-` : ''}${c.line}${c.side === 'LEFT' ? ' (old code)' : ''}`

// The comments the AI drafted for the GitHub review, to edit and post as one review with the
// maintainer's verdict. Every change is saved as soon as a field loses focus.
export function PostingPanel({ review, headSha }: { review: Review; headSha: string }) {
  const [posting, setPosting] = useState<Posting>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [event, setEvent] = useState<ReviewEvent>('COMMENT')
  const [confirming, setConfirming] = useState(false)

  // run saves a change and says whether it worked: on an error, what the user typed must stay.
  const run = (action: Promise<Posting>): Promise<boolean> => {
    setBusy(true)
    setError(undefined)
    return action
      .then((p) => {
        setPosting(p)
        return true
      })
      .catch((e) => {
        setError(e.message)
        return false
      })
      .finally(() => setBusy(false))
  }
  useEffect(() => {
    getPosting(review.id)
      .then(setPosting)
      .catch((e) => setError(e.message))
  }, [review.id])

  if (!posting) return error ? <p className="error">{error}</p> : <p className="meta">Loading the comments…</p>
  const posted = posting.posted
  const inline = posting.comments.filter((c) => c.inDiff)
  const offDiff = posting.comments.filter((c) => !c.inDiff)

  if (posted) {
    return (
      <div className="posting">
        <p>
          Posted to GitHub as {verdictText(posting.postedEvent)}
          {posting.postedUrl ? (
            <>
              :{' '}
              <a href={posting.postedUrl} target="_blank" rel="noreferrer">
                the review
              </a>
            </>
          ) : (
            '. Its link wasn\u2019t recorded: see the PR on GitHub.'
          )}
        </p>
      </div>
    )
  }
  return (
    <div className="posting">
      {review.headSha !== headSha && (
        <p className="error">
          The author pushed commits since this review. The comments are tied to the reviewed commit: GitHub shows the ones
          that no longer match the code as outdated.
        </p>
      )}
      <label className="context">
        Main comment
        <Saved
          key={posting.main}
          value={posting.main}
          rows={4}
          disabled={busy}
          onSave={(body) => run(setMainComment(review.id, body))}
        />
      </label>
      {posting.comments.length === 0 && <p className="meta">No inline comments.</p>}
      {[...inline, ...offDiff].map((c) => (
        <div key={c.id} className="draft">
          <div className="meta">
            <code>{where(c)}</code>
            {!c.inDiff && ' · Not on a line of the diff: posted in the main comment'}{' '}
            <button onClick={() => run(deleteComment(review.id, c.id))} disabled={busy} aria-label={`Delete the comment on ${where(c)}`}>
              Delete
            </button>
          </div>
          <Saved
            key={c.body}
            value={c.body}
            rows={3}
            disabled={busy}
            label={`Comment on ${where(c)}`}
            onSave={(body) => run(setComment(review.id, c.id, body))}
          />
        </div>
      ))}
      <AddComment busy={busy} onAdd={(c) => run(addComment(review.id, c))} />
      <div className="post">
        {!posting.postingEnabled ? (
          <p className="meta">
            Posting is off: set <code>GH_REVIEW_TOKEN</code>, a token that can write PR reviews, in <code>.env</code>,
            and restart the api.
          </p>
        ) : confirming ? (
          <p>
            Post {inline.length} inline comment{inline.length === 1 ? '' : 's'} and the main comment
            {offDiff.length > 0 && ` (with ${offDiff.length} comment${offDiff.length === 1 ? '' : 's'} outside the diff)`} to{' '}
            {review.repo}#{review.number} as <strong>{verdictText(event)}</strong>, as {posting.postsAs || 'the token’s user'}?
            Each comment ends with a line saying it was drafted with AI.{' '}
            <button onClick={() => run(postReview(review.id, event)).then((ok) => ok && setConfirming(false))} disabled={busy}>
              Post now
            </button>{' '}
            <button onClick={() => setConfirming(false)} disabled={busy}>
              Back
            </button>
          </p>
        ) : (
          <p>
            <label>
              Verdict{' '}
              <select value={event} onChange={(e) => setEvent(e.target.value as ReviewEvent)}>
                {verdicts.map((v) => (
                  <option key={v.value} value={v.value}>
                    {v.label}
                  </option>
                ))}
              </select>
            </label>{' '}
            <button onClick={() => setConfirming(true)} disabled={busy}>
              Post to GitHub…
            </button>
          </p>
        )}
      </div>
      {error && <p className="error">{error}</p>}
    </div>
  )
}

// Saved is a text area that saves its text when it loses focus, if it changed. Its parent gives
// it the saved value as key, so a new value starts a fresh text area.
function Saved({
  value,
  rows,
  disabled,
  label,
  onSave,
}: {
  value: string
  rows: number
  disabled: boolean
  label?: string
  onSave: (v: string) => void
}) {
  const [text, setText] = useState(value)
  return (
    <textarea
      value={text}
      rows={rows}
      disabled={disabled}
      aria-label={label}
      onChange={(e) => setText(e.target.value)}
      onBlur={() => text !== value && onSave(text)}
    />
  )
}

function AddComment({
  busy,
  onAdd,
}: {
  busy: boolean
  onAdd: (c: { path: string; line: number; side: Side; body: string }) => Promise<boolean>
}) {
  const [path, setPath] = useState('')
  const [line, setLine] = useState('')
  const [side, setSide] = useState<Side>('RIGHT')
  const [body, setBody] = useState('')
  const submit = (e: FormEvent) => {
    e.preventDefault()
    onAdd({ path: path.trim(), line: Number(line), side, body }).then((ok) => {
      if (ok) {
        setLine('')
        setBody('')
      }
    })
  }
  return (
    <form className="add-comment" onSubmit={submit}>
      <input value={path} onChange={(e) => setPath(e.target.value)} placeholder="path/to/file.go" aria-label="File" />
      <input value={line} onChange={(e) => setLine(e.target.value)} placeholder="line" inputMode="numeric" aria-label="Line" size={5} />
      <select value={side} onChange={(e) => setSide(e.target.value as Side)} aria-label="Side">
        <option value="RIGHT">new code</option>
        <option value="LEFT">old code</option>
      </select>
      <textarea value={body} onChange={(e) => setBody(e.target.value)} rows={2} aria-label="New comment" placeholder="Your comment" />
      <button type="submit" disabled={busy || !path.trim() || !Number(line) || !body.trim()}>
        Add a comment
      </button>
    </form>
  )
}
