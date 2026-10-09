package evidence

// RestateProblems is RestateWithDispositions without dispositions (CRW-828): the product restates with dispositions, and the tests pin the plain restatement.

func RestateProblems(head string, record, snapshot any) []Problem {
	problems, _ := RestateWithDispositions(head, record, snapshot, nil)
	return problems
}
