//go:build unix

package hookswitch

// exchange swaps two directory entries in one step (renameat2 RENAME_EXCHANGE on Linux, renamex_np
// RENAME_SWAP on Darwin), whatever kinds they are. KeepAside and Restore use it where the switch
// path holds a directory: a rename of a file cannot replace one, and moving the directory away first
// would leave the path empty, which a hook reads as off. It is a variable so a test can inject a
// platform or filesystem that cannot swap. Where it is not supported it returns an error and
// changes nothing.
var exchange = exchangeEntries
