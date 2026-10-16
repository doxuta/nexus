package bpm

import "fmt"

// The two lanes of an instance (specs/043 D-10, S12b). An approval is 'exclusive': at most one is
// open per record across every process. A tracking instance (an SLA clock running beside the
// approval) is 'independent': at most one per process per record, only machine nodes, never in the
// Duyệt tab / inbox, never withdrawable.
const (
	LaneExclusive   = "exclusive"
	LaneIndependent = "independent"
)

// laneOrDefault is the lane column value for a lane name ("" = exclusive).
func laneOrDefault(l string) string {
	if l == LaneIndependent {
		return LaneIndependent
	}
	return LaneExclusive
}

// IsIndependent reports whether the options put the process on the tracking lane.
func (o *Options) IsIndependent() bool { return o != nil && o.Lane == LaneIndependent }

// Lane is the lane the pinned definition runs on.
func (d *Definition) Lane() string {
	if d != nil && d.Options.IsIndependent() {
		return LaneIndependent
	}
	return LaneExclusive
}

// ErrTrackingLane: a tracking-lane instance has no submitter to withdraw it — it follows its record.
var ErrTrackingLane = fmt.Errorf("a tracking-lane instance cannot be withdrawn")

// CheckLane says why an independent-lane process cannot run (key to blame, reason); ("", "") =
// fine or not independent. kinds are the node kinds of the graph (nil = no graph form). The lane
// holds machine nodes only (nobody to hold a human step) and starts from a record event, not a
// manual send. ONE body for the API and the engine.
func CheckLane(o *Options, trigger Trigger, kinds []NodeKind) (field, reason string) {
	if !o.IsIndependent() {
		return "", ""
	}
	if kinds == nil {
		return "graph", "làn theo dõi cần quy trình dạng đồ thị (chỉ bước chờ/tự động/rẽ nhánh/kết thúc)"
	}
	if trigger == TriggerManual {
		return "trigger", "làn theo dõi chạy theo sự kiện bản ghi, không có nút gửi tay"
	}
	for i, k := range kinds {
		if !MachineKind(k) {
			return fmt.Sprintf("graph.nodes[%d].kind", i), fmt.Sprintf("làn theo dõi không có bước người (%q) — chỉ bước chờ/tự động/rẽ nhánh/kết thúc", k)
		}
	}
	return "", ""
}

// GraphKinds lists the node kinds of g in order (nil for a nil graph) — CheckLane's input.
func GraphKinds(g *Graph) []NodeKind {
	if g == nil {
		return nil
	}
	out := make([]NodeKind, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = n.Kind
	}
	return out
}
