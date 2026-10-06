package role

// dispatch_record.go exposes the ledger's own reading of a stored dispatch record to a caller that must not be
// looser than the store: the state-copy preflight of internal/runtime/install/migrate, which has to decide whether a
// retained record can be copied. The record bytes are the caller's; nothing here reads or writes a file.

import "errors"

// DispatchRecordStatuses reads the bytes of one dispatch record with the store's own reader (dispatchPinnedDecode,
// dispatch_ledger.go:432) and reports the record status and the status of every attempt, in file order. session and
// id are the identity the record must carry, exactly as dispatchPinnedRead passes them, so a record whose identity
// disagrees with its path is an error here too. A record the reader refuses returns its error unchanged; a caller
// that copies state must refuse on it rather than trust its own, looser shape check.
func DispatchRecordStatuses(data []byte, session, id string) (record string, attempts []string, err error) {
	d, err := dispatchPinnedDecode(data, session, id)
	if err != nil {
		return "", nil, err
	}
	if record, err = dispatchStatusText(d.Status); err != nil {
		return "", nil, err
	}
	attempts = make([]string, 0, len(d.Attempts))
	for _, a := range d.Attempts {
		status, err := dispatchStatusText(a.Status)
		if err != nil {
			return "", nil, err
		}
		attempts = append(attempts, status)
	}
	return record, attempts, nil
}

// dispatchStatusText is the text of a status value dispatchPinnedDecode accepted. The decoder already refused a status
// outside the store's set, so this reads one of those names; a value that is not text is an error rather than a guess.
func dispatchStatusText(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errors.New("dispatch status is not text")
	}
	return s, nil
}
