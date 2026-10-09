package swapgate

// Decide and ZoneArrivalOnly are the forms of the gate's decision that the external tests of this package read (CRW-828): the product decides with DecideWithRelease and AdditiveArrivalOnly.

// ZoneArrivalOnly is whether the swap refuses for one reason only: the candidate brings the additive
// DAG zone to a store that lacks it, with the daemon stopped and no attempt open both established.
// Anything else (a running daemon, an open attempt, a cell that could not be read, another schema
// difference) is not the arrival alone, and no backup is taken for it.
func ZoneArrivalOnly(cells map[string]Object) bool {
	return arrivalOnly(cells, ExtendsZone)
}

// Decide is swapgate.decide: the verdict, and which cells produced it. An established refusal
// is reported as a refusal even when another cell could not answer.
func Decide(cells map[string]Object) Object {
	return DecideWithRelease(cells, nil)
}
