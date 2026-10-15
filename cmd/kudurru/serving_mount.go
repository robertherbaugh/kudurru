package main

// ready reports continuing eligibility of the complete publication. The
// publisher's Initialized channel is only a historical startup signal.
func (s *servingRuntime) ready() (bool, []string) {
	status := s.publisher.Status()
	if status.ProtectedReady {
		return true, nil
	}
	return false, []string{"complete serving generation unavailable: " + status.Reason}
}
