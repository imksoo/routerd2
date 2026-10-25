package resourceprogram

// Truth distinguishes an observed false condition from missing or stale data.
// An indeterminate guard must never authorize a resource to start.
type Truth uint8

const (
	Indeterminate Truth = iota
	False
	True
)

func (t Truth) Not() Truth {
	switch t {
	case True:
		return False
	case False:
		return True
	default:
		return Indeterminate
	}
}

// All and Any use three-valued logic: false dominates conjunction and true
// dominates disjunction, even if another input is indeterminate.
func All(values ...Truth) Truth {
	if len(values) == 0 {
		return Indeterminate
	}
	result := True
	for _, v := range values {
		if v == False {
			return False
		}
		if v != True {
			result = Indeterminate
		}
	}
	return result
}

func Any(values ...Truth) Truth {
	if len(values) == 0 {
		return Indeterminate
	}
	result := False
	for _, v := range values {
		if v == True {
			return True
		}
		if v != False {
			result = Indeterminate
		}
	}
	return result
}

// FactScope identifies the source and projection associated with a view.
// The coordinator accepts observations for this boot and source; the timeline
// resets continuity when source authority changes.
type FactScope struct {
	BootID               string
	ConfigDigest         string
	OperatorRevision     uint64
	ProjectionGeneration uint64
}

// Fact is an observation already accepted by its owning coordinator. The
// original boot-time interval and real producer identity survive projection.
type Fact struct {
	Value            Truth
	ObservedNanos    uint64
	ExpiresNanos     uint64
	WorkerGeneration uint64
	ProducerEpoch    string
}
