import { useState, type FormEvent } from 'react'
import Markdown from 'react-markdown'
import { PostingPanel } from './Posting'
import {
  cancelReview,
  isActive,
  startReview,
  type PullRequest,
  type Review,
  type ReviewOptions,
} from './api'

const statusText: Record<Review['status'], string> = {
  queued: 'Waiting for the worker',
  preparing: 'Downloading the code',
  running: 'Running',
  done: 'Done',
  failed: 'Failed',
  canceled: 'Canceled',
  capped: 'Stopped at the token cap',
}

const n = (x: number) => x.toLocaleString('en')

// The review controls of one PR: the latest review's status and result, and a form to start one.
type Props = { pr: PullRequest; options?: ReviewOptions; latest?: Review; workerConnected: boolean }

export function ReviewPanel({ pr, options, latest, workerConnected }: Props) {
  const [open, setOpen] = useState(false)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [postingOpen, setPostingOpen] = useState(false)

  const run = (action: Promise<unknown>, after?: () => void) => {
    setBusy(true)
    setError(undefined)
    action
      .then(after)
      .catch((e) => setError(e.message))
      .finally(() => setBusy(false))
  }

  const active = latest && isActive(latest)
  return (
    <div className="review">
      {latest && (
        <div className="meta">
          Review: <strong>{statusText[latest.status]}</strong> · {latest.model}, {latest.effort} effort ·{' '}
          {n(latest.tokensUsed)} / {n(latest.tokenCap)} tokens
          {latest.headSha !== pr.headSha && ' · on an older commit'}
          {active && (
            <>
              {' '}
              <button onClick={() => run(cancelReview(latest.id))} disabled={busy || latest.cancelRequested}>
                {latest.cancelRequested ? 'Canceling…' : 'Cancel'}
              </button>
            </>
          )}
        </div>
      )}
      {(latest?.status === 'queued' || latest?.status === 'preparing') && !workerConnected && (
        <p className="error">
          The worker isn't connected to the api, so this review can't start. Check its logs with{' '}
          <code>docker compose logs worker</code>.
        </p>
      )}
      {latest?.error && <p className="error">{latest.error}</p>}
      {latest?.result && (
        <details open={!active}>
          <summary>{active ? 'Partial output' : 'Review'}</summary>
          <div className="markdown">
            <Markdown>{latest.result}</Markdown>
          </div>
        </details>
      )}
      {latest && !active && latest.tokensUsed > 0 && (
        // The panel loads the comments, so it's only there once the section is opened.
        <details onToggle={(e) => setPostingOpen(e.currentTarget.open)}>
          <summary>{latest.postedUrl ? 'Posted to GitHub' : 'Comments for GitHub'}</summary>
          {postingOpen && <PostingPanel review={latest} headSha={pr.headSha} />}
        </details>
      )}
      {latest && !active && latest.tokensUsed > 0 && <ContinueHint id={latest.id} />}
      {!active && !open && options && (
        <button onClick={() => setOpen(true)}>{latest ? 'Review again' : 'Review'}</button>
      )}
      {!active && open && options && (
        <ReviewForm
          options={options}
          busy={busy}
          onCancel={() => setOpen(false)}
          onStart={(req) => run(startReview({ prId: pr.id, ...req }), () => setOpen(false))}
        />
      )}
      {error && <p className="error">{error}</p>}
    </div>
  )
}

// The command that opens a review's claude session again, in a terminal, for follow-up questions.
function ContinueHint({ id }: { id: number }) {
  const [copied, setCopied] = useState(false)
  const command = `make continue REVIEW=${id}`
  const copy = () =>
    navigator.clipboard.writeText(command).then(
      () => setCopied(true),
      () => setCopied(false),
    )
  return (
    <p className="meta">
      Ask follow-up questions in a terminal, in the repo folder: <code>{command}</code>{' '}
      <button onClick={copy}>{copied ? 'Copied' : 'Copy'}</button>
      <br />
      It downloads the code again and opens this review's claude session, with the same read-only tools. claude asks
      whether you trust the folder: choose Yes, the review's flags still ignore the PR's settings, hooks, CLAUDE.md and
      MCP servers. It uses your Claude subscription.
    </p>
  )
}

type FormValues = { model: string; effort: string; tokenCap: number; context: string }

function ReviewForm({
  options,
  busy,
  onStart,
  onCancel,
}: {
  options: ReviewOptions
  busy: boolean
  onStart: (v: FormValues) => void
  onCancel: () => void
}) {
  const [model, setModel] = useState(options.defaultModel)
  const [effort, setEffort] = useState(options.defaultEffort)
  const [tokenCap, setTokenCap] = useState(options.defaultTokenCap)
  const [context, setContext] = useState('')

  const submit = (e: FormEvent) => {
    e.preventDefault()
    onStart({ model, effort, tokenCap, context })
  }
  return (
    <form className="review-form" onSubmit={submit}>
      <label>
        Model{' '}
        <select value={model} onChange={(e) => setModel(e.target.value)}>
          {options.models.map((m) => (
            <option key={m.value} value={m.value}>
              {m.label}
            </option>
          ))}
        </select>
      </label>
      <label>
        Effort{' '}
        <select value={effort} onChange={(e) => setEffort(e.target.value)}>
          {options.efforts.map((x) => (
            <option key={x}>{x}</option>
          ))}
        </select>
      </label>
      <label>
        Token cap{' '}
        <input
          type="number"
          min={options.minTokenCap}
          max={options.maxTokenCap}
          step={10_000}
          value={tokenCap}
          onChange={(e) => setTokenCap(Number(e.target.value))}
        />
      </label>
      <span className="meta">Cache reads are not counted.</span>
      <label className="context">
        Context for the AI (optional)
        <textarea
          value={context}
          maxLength={options.maxContextLength}
          rows={3}
          placeholder="For example: focus on the retry logic, the migration was agreed in the issue…"
          onChange={(e) => setContext(e.target.value)}
        />
      </label>
      <div>
        <button type="submit" disabled={busy}>
          Start the review
        </button>{' '}
        <button type="button" onClick={onCancel} disabled={busy}>
          Close
        </button>
      </div>
    </form>
  )
}
