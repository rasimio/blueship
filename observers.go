package blueship

import "github.com/rasimio/blueship/internal/core"

// Observer hooks: see internal/core/observers.go. Re-exported here rather
// than in facade.go so the whole feature is one place to read.
type (
	StartSeen        = core.StartSeen
	StartObserver    = core.StartObserver
	DeliverySeen     = core.DeliverySeen
	DeliveryObserver = core.DeliveryObserver
	PaymentApproved  = core.PaymentApproved
	PaymentObserver  = core.PaymentObserver
)

const (
	DeliveryDenial      = core.DeliveryDenial
	DeliveryHostCommand = core.DeliveryHostCommand
)
