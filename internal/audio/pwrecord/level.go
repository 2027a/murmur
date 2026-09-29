package pwrecord

import (
	"context"
	"os"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
)

func (r *recording) watchLevels(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var source *os.File
	defer func() {
		if source != nil {
			_ = source.Close()
		}
	}()
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.procDone:
			return
		case <-ticker.C:
			if source == nil {
				var err error
				source, err = wav.OpenGrowing(r.path)
				if err != nil {
					continue
				}
			}
			level, end, err := wav.MeasureGrowing(source, offset)
			if err != nil || end == offset {
				continue
			}
			offset = end
			level.UpdatedMS = time.Now().UnixMilli()
			// Do not emit buffered audio after physical capture termination.
			select {
			case <-r.procDone:
				return
			default:
			}
			r.opts.Meter(level)
		}
	}
}
