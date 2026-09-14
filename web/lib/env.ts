// Configuration, read once and validated all at once.
//
// The pattern is lifted from goal-engine/internal/config: collect every problem and
// refuse to serve, rather than failing on the first one and making an operator fix
// five things in five restarts. The check happens on the first call rather than at
// process start — Next owns the boot — so a misconfigured console starts and then
// fails every request, with this message in its log, until it is fixed. That is
// better than booting half-configured, because a half-configured console looks like
// it is working.

const DEFAULTS = {
  operatorTtlMinutes: 30,
  pollIntervalMs: 4000,
  upstreamTimeoutMs: 15_000,
} as const

// Floors, not suggestions. Zero polling means a dashboard frozen at whatever it
// showed when it loaded, and zero timeout means a request that never returns.
const FLOORS = {
  operatorTtlMinutes: 1,
  pollIntervalMs: 1000,
  upstreamTimeoutMs: 1000,
} as const

const CEILINGS = {
  // Eight hours. Beyond a working day, "temporarily elevated" stops being true.
  operatorTtlMinutes: 480,
} as const

export type Config = {
  goalEngineBaseURL: string
  coreBaseURL: string
  goalEngineBotKey: string
  cookieKey: Buffer
  operatorTtlMinutes: number
  pollIntervalMs: number
  upstreamTimeoutMs: number
}

let cached: Config | null = null

/** config returns the validated configuration, reading the environment once. */
export function config(): Config {
  if (cached) return cached
  cached = load(process.env)
  return cached
}

/**
 * load validates a raw environment. Exported for the tests, which is also why it
 * takes the environment as an argument instead of reading the global.
 */
export function load(env: Record<string, string | undefined>): Config {
  const problems: string[] = []

  const url = (name: string): string => {
    const raw = (env[name] ?? '').trim()
    if (!raw || raw === 'CHANGE_ME') {
      problems.push(`${name} is required`)
      return ''
    }
    try {
      const parsed = new URL(raw)
      if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
        problems.push(`${name} must be an http or https URL`)
      }
      // Trailing slashes make every joined path double-slashed, which some routers
      // treat as a different path.
      return raw.replace(/\/+$/, '')
    } catch {
      problems.push(`${name} must be a URL`)
      return ''
    }
  }

  const secret = (name: string): string => {
    const raw = (env[name] ?? '').trim()
    if (!raw || raw === 'CHANGE_ME') {
      problems.push(`${name} is required`)
      return ''
    }
    return raw
  }

  const bounded = (name: string, fallback: number, floor: number, ceiling?: number): number => {
    const raw = (env[name] ?? '').trim()
    if (!raw) return fallback
    const value = Number(raw)
    if (!Number.isFinite(value)) {
      problems.push(`${name} must be a number`)
      return fallback
    }
    if (value < floor) {
      problems.push(`${name} must be at least ${floor}`)
      return fallback
    }
    if (ceiling !== undefined && value > ceiling) {
      problems.push(`${name} must be at most ${ceiling}`)
      return fallback
    }
    return Math.floor(value)
  }

  const goalEngineBaseURL = url('GOAL_ENGINE_BASE_URL')
  const coreBaseURL = url('CORE_BASE_URL')
  const goalEngineBotKey = secret('GOAL_ENGINE_BOT_KEY')
  const cookieSecret = secret('WEB_COOKIE_SECRET')

  // Annotated rather than inferred: Buffer.alloc gives Buffer<ArrayBuffer> and decodeKey
  // gives Buffer<ArrayBufferLike>, and the narrower inference from the initialiser would
  // reject the decoded key.
  const cookieKey: Buffer = cookieSecret === '' ? Buffer.alloc(0) : decodeKey(cookieSecret)
  // Only when a secret was actually given — an absent one already pushed its own problem,
  // and a second line about 32 bytes would send an operator looking for the wrong mistake.
  if (cookieSecret !== '' && cookieKey.length !== 32) {
    problems.push('WEB_COOKIE_SECRET must decode to 32 bytes (openssl rand -base64 32)')
  }

  const cfg: Config = {
    goalEngineBaseURL,
    coreBaseURL,
    goalEngineBotKey,
    cookieKey,
    operatorTtlMinutes: bounded(
      'WEB_OPERATOR_TTL_MINUTES',
      DEFAULTS.operatorTtlMinutes,
      FLOORS.operatorTtlMinutes,
      CEILINGS.operatorTtlMinutes,
    ),
    pollIntervalMs: bounded('WEB_POLL_INTERVAL_MS', DEFAULTS.pollIntervalMs, FLOORS.pollIntervalMs),
    upstreamTimeoutMs: bounded(
      'WEB_UPSTREAM_TIMEOUT_MS',
      DEFAULTS.upstreamTimeoutMs,
      FLOORS.upstreamTimeoutMs,
    ),
  }

  if (problems.length > 0) {
    // The names of the variables, never their values. This message ends up in a log.
    throw new Error(`wingman web is not configured: ${problems.join('; ')}`)
  }
  return cfg
}

/** decodeKey reads a key as hex if it looks like hex, otherwise as base64. */
function decodeKey(raw: string): Buffer {
  if (/^[0-9a-fA-F]{64}$/.test(raw)) return Buffer.from(raw, 'hex')
  return Buffer.from(raw, 'base64')
}

/**
 * publicConfig is the subset a browser is allowed to know: how often to poll. It
 * exists so that no component is ever tempted to reach for config() — which holds two
 * credentials — from code that might end up in a bundle.
 */
export function publicConfig(): { pollIntervalMs: number } {
  return { pollIntervalMs: config().pollIntervalMs }
}
