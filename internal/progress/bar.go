package progress

import (
	"os"

	"github.com/schollz/progressbar/v3"
)

// Bar wraps progressbar.ProgressBar with convenience methods.
type Bar struct {
	pb    *progressbar.ProgressBar
	total int64
}

// NewSpinner creates an indeterminate spinner bar.
func NewSpinner(desc string) *Bar {
	pb := progressbar.NewOptions(-1,
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetDescription(desc),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionOnCompletion(func() {
			os.Stderr.WriteString("\n")
		}),
	)
	return &Bar{pb: pb, total: -1}
}

// SetDescription updates the bar label.
func (b *Bar) SetDescription(desc string) {
	b.pb.Describe(desc)
}

// SetTotal switches from spinner to determinate mode with the given max bytes.
func (b *Bar) SetTotal(total int64) {
	if b.total == total {
		return
	}
	b.total = total
	b.pb.ChangeMax64(total)
}

// SetCurrent updates the current value (bytes transferred).
func (b *Bar) SetCurrent(current int64) {
	_ = b.pb.Set64(current)
}

// Finish completes and clears the bar.
func (b *Bar) Finish() {
	_ = b.pb.Finish()
}
