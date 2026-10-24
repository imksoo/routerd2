package cli

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

// Fetch every bounded page before writing. A projection change must return an
// explicit retry error, never a plausible list assembled from different states.
func showResources(ctx context.Context, client *localapi.Client, out io.Writer) error {
	var result runtimeplan.ResourceObservationPage
	if err := client.Do(ctx, "GET", "/v1/services/resources", nil, &result); err != nil {
		return err
	}
	for result.NextOffset != nil {
		offset := *result.NextOffset
		if offset != len(result.Resources) || offset >= result.Total {
			return errors.New("invalid resource page continuation")
		}
		query := url.Values{"offset": {strconv.Itoa(offset)}, "observation_id": {result.ObservationID}}
		var next runtimeplan.ResourceObservationPage
		if err := client.Do(ctx, "GET", "/v1/services/resources?"+query.Encode(), nil, &next); err != nil {
			return err
		}
		if next.ObservationID != result.ObservationID || next.Total != result.Total || next.Offset != offset || len(next.Resources) == 0 {
			return errors.New("resource observation changed; retry show resources")
		}
		result.Resources = append(result.Resources, next.Resources...)
		result.NextOffset = next.NextOffset
	}
	if len(result.Resources) != result.Total {
		return errors.New("incomplete resource observation")
	}
	return WriteJSON(out, result)
}
