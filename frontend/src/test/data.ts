// Sample API data for the tests.
import type { PullRequest, Review, ReviewOptions } from '../api'

export const pr: PullRequest = {
  id: 'PR_1',
  repo: 'o/r',
  number: 7,
  title: 'Add retries',
  url: 'https://github.com/o/r/pull/7',
  author: 'octo',
  isDraft: false,
  createdAt: new Date(Date.now() - 3 * 86_400_000).toISOString(),
  updatedAt: new Date(Date.now() - 2 * 3_600_000).toISOString(),
  additions: 10,
  deletions: 2,
  changedFiles: 3,
  headSha: 'abc',
  ciState: 'FAILURE',
  reasons: [{ kind: 'requested', viaTeam: true, teams: ['o/team'] }],
}

export const options: ReviewOptions = {
  models: [
    { value: 'sonnet', label: 'Sonnet' },
    { value: 'haiku', label: 'Haiku' },
  ],
  efforts: ['low', 'medium', 'high'],
  defaultModel: 'sonnet',
  defaultEffort: 'medium',
  defaultTokenCap: 500_000,
  minTokenCap: 10_000,
  maxTokenCap: 2_000_000,
  maxContextLength: 2000,
}

export const review = (over: Partial<Review> = {}): Review => ({
  id: 1,
  prId: 'PR_1',
  repo: 'o/r',
  number: 7,
  headSha: 'abc',
  model: 'sonnet',
  effort: 'medium',
  tokenCap: 500_000,
  context: '',
  status: 'running',
  cancelRequested: false,
  sessionId: 's',
  tokensUsed: 1234,
  result: '',
  error: '',
  createdAt: new Date().toISOString(),
  startedAt: null,
  finishedAt: null,
  ...over,
})
