import type { PullRequest, QueueResponse, Reason, Review, ReviewOptions } from './api'
import { ReviewPanel } from './ReviewPanel'

const ago = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })

// "3 days ago" style text for an ISO date.
function timeAgo(iso: string): string {
  const seconds = (new Date(iso).getTime() - Date.now()) / 1000
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['year', 31_536_000],
    ['month', 2_592_000],
    ['day', 86_400],
    ['hour', 3_600],
    ['minute', 60],
  ]
  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size) return ago.format(Math.round(seconds / size), unit)
  }
  return 'just now'
}

function reasonText(r: Reason): string {
  switch (r.kind) {
    case 'requested':
      return r.viaTeam ? 'Review requested from a team' : 'Review requested from you'
    case 'watched':
      return 'Watched repo'
    case 'new_commits':
      return 'New commits since your review'
  }
}

const ciText: Record<string, string> = {
  SUCCESS: 'CI passed',
  FAILURE: 'CI failed',
  ERROR: 'CI failed',
  PENDING: 'CI running',
  EXPECTED: 'CI waiting',
}

type RowProps = { pr: PullRequest; options?: ReviewOptions; latest?: Review; workerConnected: boolean }

function Row({ pr, options, latest, workerConnected }: RowProps) {
  return (
    <li className="pr">
      <div>
        <a href={pr.url} target="_blank" rel="noreferrer">
          {pr.repo}#{pr.number}
        </a>{' '}
        <strong>{pr.title}</strong>
        {pr.isDraft && <span className="tag">Draft</span>}
      </div>
      <div className="meta">
        by {pr.author} · opened {timeAgo(pr.createdAt)} · updated {timeAgo(pr.updatedAt)} ·{' '}
        <span className="add">+{pr.additions}</span> <span className="del">−{pr.deletions}</span> in{' '}
        {pr.changedFiles} files
        {pr.ciState && (
          <>
            {' · '}
            <span className={`ci ci-${pr.ciState.toLowerCase()}`}>{ciText[pr.ciState] ?? pr.ciState}</span>
          </>
        )}
      </div>
      <div>
        {pr.reasons.map((r, i) => (
          <span
            key={i}
            className={`tag reason-${r.kind}`}
            title={r.kind === 'requested' && r.teams?.length ? `Teams requested on this PR: ${r.teams.join(', ')}` : undefined}
          >
            {reasonText(r)}
          </span>
        ))}
      </div>
      <ReviewPanel pr={pr} options={options} latest={latest} workerConnected={workerConnected} />
    </li>
  )
}

type Props = {
  queue?: QueueResponse
  reviewOptions?: ReviewOptions
  latestReviews: Map<string, Review> // by PR id
  workerConnected: boolean
  loadError?: string
  refreshing: boolean
  onRefresh: () => void
}

export function Queue({ queue, reviewOptions, latestReviews, workerConnected, loadError, refreshing, onRefresh }: Props) {
  const status = queue?.status
  return (
    <section>
      <div className="section-head">
        <h2>Waiting for you{queue && ` (${queue.items.length})`}</h2>
        <span className="meta">
          {queue?.tokenSource && queue.tokenSource !== 'default' && `GitHub token from ${queue.tokenSource} · `}
          {status?.refreshedAt && `Checked ${timeAgo(status.refreshedAt)} `}
          <button onClick={onRefresh} disabled={refreshing}>
            {refreshing ? 'Refreshing…' : 'Refresh'}
          </button>
        </span>
      </div>
      {loadError && <p className="error">Could not load the queue: {loadError}</p>}
      {status?.error && <p className="error">Last GitHub check failed: {status.error}</p>}
      {queue && queue.items.length === 0 && !status?.error && <p>Nothing is waiting for you.</p>}
      <ul className="prs">
        {queue?.items.map((pr) => (
          <Row
            key={pr.id}
            pr={pr}
            options={reviewOptions}
            latest={latestReviews.get(pr.id)}
            workerConnected={workerConnected}
          />
        ))}
      </ul>
    </section>
  )
}
