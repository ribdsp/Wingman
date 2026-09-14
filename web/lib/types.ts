// The shapes the two services send, as they send them.
//
// camelCase, unchanged. No transform layer, no snake_case round trip: the services
// publish camelCase because that is the house convention, and a console that renamed
// fields on the way in would be a second vocabulary to keep in step with docs/api.md.
//
// Optional fields are the ones the Go views mark omitempty; `| null` is the ones the Go
// views render as an explicit null. The distinction is kept because the services keep it.
//
// The three readers at the bottom exist because three fields are dangerous to misread.
// Everything else is typed and trusted; those three are narrowed.

import { bool, isRecord, num, str } from './envelope'

// ---- goal engine ---------------------------------------------------------------

export type Goal = {
  id: string
  product: string
  title: string
  sourceText?: string
  metricKey: string
  comparator: string
  targetValue: number
  baselineValue: number | null
  periodStart: string
  periodEnd: string
  status: string
  toleranceRatio: number
  triggerCooldownSeconds: number
  maxTriggersPerPeriod: number
  botId?: string
  channelId?: string
  createdBy: string
  createdAt: string
  updatedAt?: string
}

/**
 * Evaluation is one recorded verdict. Every number the evaluator used is published, so
 * nothing here recomputes pace — see docs/goal-engine.md, "Do not recompute pace".
 */
export type Evaluation = {
  id: string
  goalId: string
  sampleId?: number
  observedValue: number
  targetValue: number
  baselineValue: number
  expectedValue: number
  progressRatio: number
  elapsedRatio: number
  paceRatio: number
  onTrack: boolean
  targetMet: boolean
  decision: string
  reason: string
  evaluatedAt: string
  createdAt: string
}

export type Approval = {
  id: string
  actionType: string
  amount: number
  currency: string
  requestedBy: string
  goalId: string | null
  idempotencyKey?: string
  outcome: string
  policyReason?: string
  resolution: string | null
  resolvedBy: string | null
  resolvedAt: string | null
  resolutionNote?: string
  payload?: string
  isOpen: boolean
  createdAt: string
  expiresAt: string | null
}

export type Flag = {
  key: string
  enabled: boolean
  reason?: string
  updatedBy?: string
  updatedAt: string | null
}

export type AuditEvent = {
  id: number
  at: string
  actorType: string
  actorId: string
  action: string
  subjectType: string
  subjectId: string
  outcome: string
  /** Already JSON in storage, emitted as-is rather than as a quoted string. */
  detail?: unknown
  requestId?: string
}

export type Metric = {
  key: string
  description?: string
  unit?: string
  source: string
}

export type Sample = {
  id: number
  metricKey: string
  value: number
  observedAt: string
  recordedAt: string
  source: string
}

// ---- core ----------------------------------------------------------------------

export type User = {
  id: string
  email: string
  displayName: string
  isActive: boolean
  createdAt: string
}

export type SignedIn = {
  token: string
  expiresAt: string
  user: User
}

export type Chat = {
  id: string
  title: string
  archivedAt: string | null
  isArchived: boolean
  createdAt: string
  updatedAt: string
}

export type Message = {
  id: string
  chatId: string
  runId?: string
  role: string
  content: string
  tokensIn: number
  tokensOut: number
  createdAt: string
}

export type Task = {
  id: string
  source: string
  status: string
  botId?: string
  channelId?: string
  brief: string
  idempotencyKey?: string
  metadata?: Record<string, string>
  createdAt: string
}

/** Sent is what POST /v1/messages answers with: the chat, the message, and the task. */
export type Sent = {
  chat: Chat
  message: Message
  task: Task
}

export type RunLimits = {
  maxIterations: number
  maxToolCalls: number
  maxTokensPerRun: number
  /** null when this instance sets no daily cap. Never 0, which would mean the opposite. */
  maxTokensPerUserDay: number | null
  stepTimeoutSeconds: number
  sandboxTimeoutSeconds: number
}

export type RunState = {
  iterations: number
  toolCalls: number
  tokensUsed: number
}

export type Run = {
  id: string
  taskId: string
  provider?: string
  model?: string
  /** One of the eleven stop reasons. Empty while the run is in flight. */
  stop?: string
  reason?: string
  isInFlight: boolean
  isCancelled: boolean
  limits: RunLimits
  state: RunState
  startedAt: string
  finishedAt: string | null
}

export type RunStep = {
  index: number
  kind: string
  toolName?: string
  content: string
  err?: string
  tokensIn: number
  tokensOut: number
  at: string
}

export type Spend = {
  runId: string
  provider: string
  model: string
  tokensIn: number
  tokensOut: number
  total: number
  occurredAt: string
}

export type Cost = {
  charges: Spend[]
  total: number
}

export type Ledger = {
  isReadable: boolean
  tokensToday: number
}

export type Session = {
  id: string
  userAgent: string
  createdIp: string
  createdAt: string
  expiresAt: string
  revokedAt: string | null
  lastSeenAt: string | null
  isLive: boolean
}

export type LinkCode = {
  code: string
  expiresAt: string
}

export type ChannelIdentity = {
  id: string
  kind: string
  externalId: string
  displayName: string
  linkedAt: string
  revokedAt: string | null
  isLive: boolean
}

/**
 * SwitchReading is the kill switch as the console understands it.
 *
 * `readable` is separate from `engaged` because the two are different sentences on screen —
 * "an operator engaged this" and "the engine did not answer" both stop work, and an
 * operator debugging at 2am needs to know which one they are looking at.
 */
export type SwitchReading = {
  engaged: boolean
  readable: boolean
  reason: string
  updatedBy: string
  updatedAt: string | null
}

/**
 * What the console shows when the flag cannot be read.
 *
 * Engaged. This is the engine's own rule — an unreadable kill-switch flag means engaged,
 * because unreadable means halt — and the console must not be the one place in the system
 * that reads a missing brake as a released one.
 */
export const SWITCH_UNREADABLE: SwitchReading = {
  engaged: true,
  readable: false,
  reason: '',
  updatedBy: '',
  updatedAt: null,
}

export function readSwitch(data: unknown): SwitchReading {
  if (!isRecord(data)) return SWITCH_UNREADABLE
  const enabled = bool(data.enabled)
  if (enabled === null) return SWITCH_UNREADABLE
  return {
    engaged: enabled,
    readable: true,
    reason: str(data.reason),
    updatedBy: str(data.updatedBy),
    updatedAt: typeof data.updatedAt === 'string' ? data.updatedAt : null,
  }
}

// ---- the three readings worth narrowing ----------------------------------------

/**
 * readLedger narrows core's spend reading.
 *
 * `isReadable` decides whether a number exists at all, and a missing or non-boolean flag
 * is read as unreadable rather than assumed true. A console that assumed readable would
 * render 0 spent for a broken ledger, which is the reading that invites spending more.
 */
export function readLedger(data: unknown): { isReadable: boolean | null; tokensToday: number | null } {
  if (!isRecord(data)) return { isReadable: null, tokensToday: null }
  const isReadable = bool(data.isReadable)
  if (isReadable !== true) return { isReadable, tokensToday: null }
  return { isReadable, tokensToday: num(data.tokensToday) }
}

/**
 * readLimits narrows a run's limit snapshot, keeping the uncapped/zero distinction that
 * core is careful about: null means no daily cap, zero means unset and would be a bug.
 */
export function readLimits(data: unknown): {
  maxIterations: number | null
  maxToolCalls: number | null
  maxTokensPerRun: number | null
  maxTokensPerUserDay: number | null
  stepTimeoutSeconds: number | null
  sandboxTimeoutSeconds: number | null
} {
  const raw = isRecord(data) ? data : {}
  return {
    maxIterations: num(raw.maxIterations),
    maxToolCalls: num(raw.maxToolCalls),
    maxTokensPerRun: num(raw.maxTokensPerRun),
    maxTokensPerUserDay: num(raw.maxTokensPerUserDay),
    stepTimeoutSeconds: num(raw.stepTimeoutSeconds),
    sandboxTimeoutSeconds: num(raw.sandboxTimeoutSeconds),
  }
}

/**
 * readEvaluation narrows one verdict for display.
 *
 * paceRatio is the number the whole deck is coloured by, and it comes back null when the
 * field is absent or not a number — so a goal the engine has never assessed is rendered
 * unknown rather than at zero pace.
 */
export function readEvaluation(data: unknown): {
  goalId: string
  paceRatio: number | null
  progressRatio: number | null
  elapsedRatio: number | null
  observedValue: number | null
  expectedValue: number | null
  targetValue: number | null
  onTrack: boolean | null
  targetMet: boolean | null
  decision: string
  reason: string
  evaluatedAt: string
} {
  const raw = isRecord(data) ? data : {}
  return {
    goalId: str(raw.goalId),
    paceRatio: num(raw.paceRatio),
    progressRatio: num(raw.progressRatio),
    elapsedRatio: num(raw.elapsedRatio),
    observedValue: num(raw.observedValue),
    expectedValue: num(raw.expectedValue),
    targetValue: num(raw.targetValue),
    onTrack: bool(raw.onTrack),
    targetMet: bool(raw.targetMet),
    decision: str(raw.decision),
    reason: str(raw.reason),
    evaluatedAt: str(raw.evaluatedAt),
  }
}

export type EvaluationReading = ReturnType<typeof readEvaluation>
export type LimitsReading = ReturnType<typeof readLimits>
export type LedgerReadingRaw = ReturnType<typeof readLedger>
