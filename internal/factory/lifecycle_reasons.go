package factory

// LifecycleReasonHarnessCapacityUnavailable identifies a temporary harness
// capacity pause that an explicit resume may retry.
const LifecycleReasonHarnessCapacityUnavailable = "harness capacity unavailable"

// LifecycleReasonHarnessAuthenticationExpired identifies a credential pause
// that follows an expired harness authentication failure.
const LifecycleReasonHarnessAuthenticationExpired = "harness authentication expired"

// LifecycleReasonAutomaticHarnessRecoveryExhausted identifies the boundary
// after the bounded automatic native-session recovery attempts are consumed.
const LifecycleReasonAutomaticHarnessRecoveryExhausted = "automatic harness recovery exhausted"

// LifecycleReasonCheckInfrastructureUnavailable identifies a check pause whose
// gate suite or repair launch failed for infrastructure reasons. An explicit
// resume retries the checks at the same checkpoint without spending a repair
// attempt.
const LifecycleReasonCheckInfrastructureUnavailable = "check infrastructure unavailable"
