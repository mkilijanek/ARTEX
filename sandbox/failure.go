package sandbox

// ReportFailure records a failed start-up step that is not one of the checks C1-C11 and makes the
// status degraded, with id and detail as the reason. It is for a part the server starts after
// activation, such as the egress forwarder; a repeat with the same id replaces the earlier entry.
func (i *Isolation) ReportFailure(id, detail string) {
	if i == nil {
		return
	}
	i.setCheck(CheckResult{ID: id, Detail: detail, Degrades: true})
	i.logStatus()
}
