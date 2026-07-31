package delivery

import "time"

// CommandTimeout bounds one clipboard operation or one complete injection
// cascade. Delivery timing is a lifecycle safeguard, not user policy.
const CommandTimeout = 5 * time.Second
