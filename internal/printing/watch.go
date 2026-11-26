package printing

import (
	"context"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/ippnotify"
)

// Watch subscribes to the printer's state and configuration events and calls
// changed after subscribing and on each batch of events, until ctx ends. It
// returns as ippnotify.Watch does: ctx.Err(), or an error wrapping
// ippnotify.ErrUnsupported (a printer without IPP notifications, such as
// go-mfp's) or ippnotify.ErrRefused.
func (c *Client) Watch(ctx context.Context, changed func()) error {
	return ippnotify.Watch(ctx, ippnotify.Target{
		URI: c.uri, URIAttr: "printer-uri", Op: goipp.OpCreatePrinterSubscriptions,
		Events: []string{
			ippnotify.EventPrinterStateChanged, ippnotify.EventPrinterConfigChanged,
			ippnotify.EventPrinterMediaChanged, ippnotify.EventPrinterFinishingsChanged,
			ippnotify.EventPrinterStopped, ippnotify.EventPrinterRestarted, ippnotify.EventPrinterShutdown,
		},
	}, func([]goipp.Attributes) { changed() })
}

// WatchJob subscribes to job id's state events and calls changed after
// subscribing and on each batch of events. It returns nil once the job reached
// a terminal state, and otherwise as Watch does; a job that is already gone
// gives ippnotify.ErrRefused.
func (c *Client) WatchJob(ctx context.Context, id int, changed func()) error {
	return ippnotify.Watch(ctx, ippnotify.Target{
		URI: c.uri, URIAttr: "printer-uri", Op: goipp.OpCreateJobSubscriptions, JobID: id,
		Events: []string{ippnotify.EventJobStateChanged, ippnotify.EventJobCompleted},
	}, func([]goipp.Attributes) { changed() })
}
