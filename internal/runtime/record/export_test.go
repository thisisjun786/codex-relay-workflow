package record

// StampSourceTree stands in for the build's -X stamp for one test.
func StampSourceTree(tree string) func() {
	saved := sourceTree
	sourceTree = tree
	return func() { sourceTree = saved }
}
