package report

import "time"

// Realized-savings audit (S28): what a workload cost per month before and after an applied change, from stored samples.
const (
	SavingsBefore = 7 * 24 * time.Hour // the cost before a change is the mean over this window
	SavingsSettle = time.Hour          // after a change, old pods are still replaced: samples in this hour do not count
	SavingsMin    = 24 * time.Hour     // less measured time after the change than this, and there is no number yet
)

// Point is a workload's monthly cost in the sample taken at At.
type Point struct {
	At      time.Time
	Monthly float64
}

// Realized is the audit of one change. Saving is Before minus After: negative means the cost went up.
type Realized struct {
	// "saved", "increase", "flat" or "pending" (not enough data yet); "nobaseline" (no cost before the change) and "superseded"
	// (the next change came before SavingsMin of data) are final: no later sample can make a number (S42)
	Status        string
	Before, After float64
	Saving        float64
}

// RealizedSaving joins the change at `at` with the cost points around it (oldest first). A non-zero until closes the after
// window: the next change on the same workload, so a later change is not credited to this one. The estimates stay floats; nothing here is billed.
func RealizedSaving(at, until time.Time, pts []Point) Realized {
	var b, a []Point
	for _, p := range pts {
		switch {
		case !p.At.Before(at.Add(-SavingsBefore)) && p.At.Before(at):
			b = append(b, p)
		case !p.At.Before(at.Add(SavingsSettle)) && (until.IsZero() || p.At.Before(until)):
			a = append(a, p)
		}
	}
	switch {
	case len(b) == 0:
		return Realized{Status: "nobaseline"}
	case len(a) > 0 && a[len(a)-1].At.Sub(a[0].At) >= SavingsMin:
	case !until.IsZero():
		return Realized{Status: "superseded"}
	default:
		return Realized{Status: "pending"}
	}
	r := Realized{Before: mean(b), After: mean(a)}
	r.Saving = r.Before - r.After
	switch {
	case r.Saving > 0.005:
		r.Status = "saved"
	case r.Saving < -0.005:
		r.Status = "increase"
	default:
		r.Status = "flat"
	}
	return r
}

func mean(p []Point) (s float64) {
	for _, x := range p {
		s += x.Monthly
	}
	return s / float64(len(p))
}
