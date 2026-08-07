package delivery

import "time"

// CommandTimeout bounds one clipboard operation, clear, or injection attempt.
// Delivery timing is a lifecycle safeguard, not user policy.
const CommandTimeout = 5 * time.Second
