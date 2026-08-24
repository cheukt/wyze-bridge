package wyzeapi

// connStateOnline reports whether Wyze itself considers the device reachable.
// get_object_list carries conn_state (1 = online, 0 = offline) alongside a
// *last-known* LanIP, which the cloud keeps serving after a camera drops off
// the network — so conn_state is the only field that distinguishes "dialable"
// from "we have a stale address for it". Absent field means online: a response
// shape change should not mark the whole fleet offline.
func connStateOnline(dev map[string]interface{}) bool {
	v, ok := dev["conn_state"]
	if !ok {
		return true
	}
	switch n := v.(type) {
	case float64:
		return n != 0
	case int:
		return n != 0
	case string:
		return n != "0" && n != ""
	}
	return true
}
