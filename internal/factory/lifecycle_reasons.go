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
