package hook

// sockaddrUnPathLimit is the size of sockaddr_un.sun_path on this system: a path of this many bytes or more does not fit with its
// terminating NUL (Linux 108, darwin 104; CRW-1398).
const sockaddrUnPathLimit = 104
