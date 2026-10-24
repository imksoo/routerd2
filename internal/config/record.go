package config

const maxDocumentNodes = 200000

// Persisted records are JSON, independent of the operator-facing YAML parser.
// Stream the shape check before typed decoding to reject duplicate keys and
// unbounded nesting without constructing an intermediate YAML node tree. The
// record budget includes two individually bounded documents and their metadata.
func decodeRecord(data []byte, record *Committed) error {
	return decodeStrictJSON(data, record, 2*maxDocumentNodes+1024)
}

func checkJSONShape(data []byte, maximum int) error {
	return checkTypedJSONShape(data, maximum, nil)
}
