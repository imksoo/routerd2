package config

import "github.com/imksoo/routerd2/internal/strictjson"

func decodeStrictJSON(data []byte, target any, maximum int) error {
	return strictjson.Decode(data, target, maximum)
}
