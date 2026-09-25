package provider

import (
	"context"
	"time"

	"github.com/minhthienhuynh/quirkproxy/model"
)

// Capability advertises what a provider can do.
type Capability string

const (
	// CapabilityListModels means the provider can list models.
	CapabilityListModels Capability = "list_models"
	// CapabilityChat is reserved for the future inference change.
	CapabilityChat Capability = "chat"
)

// ListResult is the outcome of one provider listing.
type ListResult struct {
	// Models passed the provider's filters.
	Models []model.Model
	// Drops records every model that was filtered out and why.
	Drops []Drop
	// Age is the age of the oldest data backing this result.
	Age time.Duration
}

// ModelLister is implemented by every provider that can list models.
type ModelLister interface {
	// ID returns the unique provider identifier (e.g. "opencode").
	ID() string
	// Capabilities returns the capabilities this provider supports.
	Capabilities() []Capability
	// ListModels returns the normalized models that passed the
	// provider's own filters.
	ListModels(ctx context.Context) (ListResult, error)
}

// ChatCompleter is reserved for the future inference change.
// It is intentionally left without methods in this change.
type ChatCompleter interface {
	chatCompleter()
}

// DropReason identifies the filter step that removed a model.
type DropReason string

const (
	// DropPrice means cost.input != 0.
	DropPrice DropReason = "price"
	// DropStatus means deprecated (or alpha without experimental).
	DropStatus DropReason = "status"
	// DropMissingCatalog means absent from the Zen catalog.
	DropMissingCatalog DropReason = "missing_catalog"
	// DropListFilter means removed by blacklist/whitelist.
	DropListFilter DropReason = "list_filter"
	// DropMissingMetadata means required metadata (e.g. cost) is absent.
	DropMissingMetadata DropReason = "missing_metadata"
)

// Drop records one model removed from the output and why.
type Drop struct {
	ModelID string
	Reason  DropReason
	Detail  string
}
