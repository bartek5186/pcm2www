// internal/integrations/types.go
package integrations

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"
)

type Integration interface {
	Name() string
	Start(ctx context.Context) error // blokuje do ctx.Done (long-running) lub odpala własną pętlę
	Stop()                           // idempotent
}

type Factory func(log zerolog.Logger, raw json.RawMessage) (Integration, error)
