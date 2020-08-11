package cockroach

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/golang/geo/s2"
	dsserr "github.com/interuss/dss/pkg/errors"
	dssmodels "github.com/interuss/dss/pkg/models"
	scderr "github.com/interuss/dss/pkg/scd/errors"
	scdmodels "github.com/interuss/dss/pkg/scd/models"
	dsssql "github.com/interuss/dss/pkg/sql"
	"github.com/lib/pq"
	"github.com/palantir/stacktrace"
)

var (
	operationFieldsWithIndices   [10]string
	operationFieldsWithPrefix    string
	operationFieldsWithoutPrefix string
)

func init() {
	operationFieldsWithIndices[0] = "id"
	operationFieldsWithIndices[1] = "owner"
	operationFieldsWithIndices[2] = "version"
	operationFieldsWithIndices[3] = "url"
	operationFieldsWithIndices[4] = "altitude_lower"
	operationFieldsWithIndices[5] = "altitude_upper"
	operationFieldsWithIndices[6] = "starts_at"
	operationFieldsWithIndices[7] = "ends_at"
	operationFieldsWithIndices[8] = "subscription_id"
	operationFieldsWithIndices[9] = "updated_at"

	operationFieldsWithoutPrefix = strings.Join(
		operationFieldsWithIndices[:], ",",
	)

	withPrefix := make([]string, len(operationFieldsWithIndices))
	for idx, field := range operationFieldsWithIndices {
		withPrefix[idx] = "scd_operations." + field
	}

	operationFieldsWithPrefix = strings.Join(
		withPrefix[:], ",",
	)
}

func (s *repo) fetchOperations(ctx context.Context, q dsssql.Queryable, query string, args ...interface{}) ([]*scdmodels.Operation, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, stacktrace.Propagate(err, "Error in query: %s", query)
	}
	defer rows.Close()

	var payload []*scdmodels.Operation
	for rows.Next() {
		var (
			o         = &scdmodels.Operation{}
			updatedAt time.Time
		)
		err := rows.Scan(
			&o.ID,
			&o.Owner,
			&o.Version,
			&o.USSBaseURL,
			&o.AltitudeLower,
			&o.AltitudeUpper,
			&o.StartTime,
			&o.EndTime,
			&o.SubscriptionID,
			&updatedAt,
		)
		if err != nil {
			return nil, stacktrace.Propagate(err, "Error scanning Operation row")
		}
		o.OVN = scdmodels.NewOVNFromTime(updatedAt, o.ID.String())
		payload = append(payload, o)
	}
	if err := rows.Err(); err != nil {
		return nil, stacktrace.Propagate(err, "Error in rows query result")
	}

	return payload, nil
}

func (s *repo) fetchOperation(ctx context.Context, q dsssql.Queryable, query string, args ...interface{}) (*scdmodels.Operation, error) {
	operations, err := s.fetchOperations(ctx, q, query, args...)
	if err != nil {
		return nil, err
	}
	if len(operations) > 1 {
		return nil, stacktrace.NewError("Query returned %d Operations when only 0 or 1 was expected", len(operations))
	}
	if len(operations) == 0 {
		return nil, sql.ErrNoRows
	}
	return operations[0], nil
}

func (s *repo) fetchOperationByID(ctx context.Context, q dsssql.Queryable, id dssmodels.ID) (*scdmodels.Operation, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM
			scd_operations
		WHERE
			id = $1`, operationFieldsWithoutPrefix)
	return s.fetchOperation(ctx, q, query, id)
}

// pushOperation creates/updates the Operation identified by "id" and owned by
// "owner", affecting "cells" in the time interval ["starts", "ends"].
//
// Returns the created/updated Operation and all Subscriptions
// affected by the operation.
func (s *repo) pushOperation(ctx context.Context, q dsssql.Queryable, operation *scdmodels.Operation) (
	*scdmodels.Operation, []*scdmodels.Subscription, error) {
	var (
		upsertOperationsQuery = fmt.Sprintf(`
			WITH v AS (
				SELECT
					version
				FROM
					scd_operations
				WHERE
					id = $1
			)
			UPSERT INTO
				scd_operations
				(%s)
			VALUES
				($1, $2, COALESCE((SELECT version from v), 0) + 1, $3, $4, $5, $6, $7, $8, transaction_timestamp())
			RETURNING
				%s`, operationFieldsWithoutPrefix, operationFieldsWithPrefix)
		upsertCellsForOperationQuery = `
			UPSERT INTO
				scd_cells_operations
				(cell_id, cell_level, operation_id)
			VALUES
				($1, $2, $3)`
		deleteLeftOverCellsForOperationQuery = `
			DELETE FROM
				scd_cells_operations
			WHERE
				cell_id != ALL($1)
			AND
				operation_id = $2`
	)

	cids := make([]int64, len(operation.Cells))
	clevels := make([]int, len(operation.Cells))

	for i, cell := range operation.Cells {
		cids[i] = int64(cell)
		clevels[i] = cell.Level()
	}

	cells := operation.Cells
	operation, err := s.fetchOperation(ctx, q, upsertOperationsQuery,
		operation.ID,
		operation.Owner,
		operation.USSBaseURL,
		operation.AltitudeLower,
		operation.AltitudeUpper,
		operation.StartTime,
		operation.EndTime,
		operation.SubscriptionID,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error fetching Operation")
	}
	operation.Cells = cells

	for i := range cids {
		if _, err := q.ExecContext(ctx, upsertCellsForOperationQuery, cids[i], clevels[i], operation.ID); err != nil {
			return nil, nil, stacktrace.Propagate(err, "Error in query: %s", upsertCellsForOperationQuery)
		}
	}

	if _, err := q.ExecContext(ctx, deleteLeftOverCellsForOperationQuery, pq.Array(cids), operation.ID); err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error in query: %s", deleteLeftOverCellsForOperationQuery)
	}

	subscriptions, err := s.fetchSubscriptionsForNotification(ctx, q, cids)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error fetching Subscriptions for notification")
	}

	return operation, subscriptions, nil
}

func (s *repo) populateOperationCells(ctx context.Context, q dsssql.Queryable, o *scdmodels.Operation) error {
	const query = `
	SELECT
		cell_id
	FROM
		scd_cells_operations
	WHERE operation_id = $1`

	rows, err := q.QueryContext(ctx, query, o.ID)
	if err != nil {
		return stacktrace.Propagate(err, "Error in query: %s", query)
	}
	defer rows.Close()

	var cell int64
	o.Cells = s2.CellUnion{}

	for rows.Next() {
		if err := rows.Scan(&cell); err != nil {
			return stacktrace.Propagate(err, "Error scanning cell ID row")
		}
		o.Cells = append(o.Cells, s2.CellID(uint64(cell)))
	}
	if err := rows.Err(); err != nil {
		return stacktrace.Propagate(err, "Error in rows query result")
	}

	return nil
}

// GetOperation returns an operation for the given ID from CockroachDB
func (s *repo) GetOperation(ctx context.Context, id dssmodels.ID) (*scdmodels.Operation, error) {
	sub, err := s.fetchOperationByID(ctx, s.q, id)
	switch err {
	case nil:
		return sub, nil
	case sql.ErrNoRows:
		return nil, stacktrace.NewErrorWithCode(dsserr.NotFound, "Operation %s not found", id.String())
	default:
		return nil, err // No need to Propagate this error as this stack layer does not add useful information
	}
}

// DeleteOperation deletes an operation for the given ID from CockroachDB
func (s *repo) DeleteOperation(ctx context.Context, id dssmodels.ID, owner dssmodels.Owner) (*scdmodels.Operation, []*scdmodels.Subscription, error) {
	var (
		deleteQuery = `
			DELETE FROM
				scd_operations
			WHERE
				id = $1
			AND
				owner = $2
		`
		deleteImplicitSubscriptionQuery = `
			DELETE FROM
				scd_subscriptions
			WHERE
				id = $1
			AND
				owner = $2
			AND
				implicit = true
			AND
				0 = ALL (
					SELECT
						COALESCE(COUNT(id),0)
					FROM
						scd_operations
					WHERE
						subscription_id = $1
				)
		`
	)

	// We fetch to know whether to return a concurrency error, or a not found error
	old, err := s.fetchOperationByID(ctx, s.q, id)
	switch {
	case err == sql.ErrNoRows: // Return a 404 here.
		return nil, nil, stacktrace.NewErrorWithCode(dsserr.NotFound, "Operation %s not found", id.String())
	case err != nil:
		return nil, nil, stacktrace.Propagate(err, "Error fetching Operation by ID")
	case old != nil && old.Owner != owner:
		return nil, nil, stacktrace.NewErrorWithCode(dsserr.PermissionDenied, "Operation is owned by different client")
	}
	if err := s.populateOperationCells(ctx, s.q, old); err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error populating Operation cells")
	}

	cids := make([]int64, len(old.Cells))
	for i, cell := range old.Cells {
		cids[i] = int64(cell)
	}
	subscriptions, err := s.fetchSubscriptionsForNotification(ctx, s.q, cids)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error fetching Subscriptions for notification")
	}

	if _, err := s.q.ExecContext(ctx, deleteQuery, id, owner); err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error in query: %s", deleteQuery)
	}
	if _, err := s.q.ExecContext(ctx, deleteImplicitSubscriptionQuery, old.SubscriptionID, owner); err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error in query: %s", deleteImplicitSubscriptionQuery)
	}

	return old, subscriptions, nil
}

// UpsertOperation inserts or updates an operation in CockroachDB
func (s *repo) UpsertOperation(ctx context.Context, operation *scdmodels.Operation, key []scdmodels.OVN) (*scdmodels.Operation, []*scdmodels.Subscription, error) {
	old, err := s.fetchOperationByID(ctx, s.q, operation.ID)
	switch {
	case err == sql.ErrNoRows:
		break
	case err != nil:
		return nil, nil, stacktrace.Propagate(err, "Error fetching Operation by ID")
	}

	switch {
	case old == nil && !operation.Version.Empty():
		// The user wants to update an existing Operation, but one wasn't found.
		return nil, nil, stacktrace.NewErrorWithCode(dsserr.NotFound, "Operation %s not found", operation.ID.String())
	case old != nil && operation.Version.Empty():
		// The user wants to create a new Operation but it already exists.
		return nil, nil, stacktrace.NewErrorWithCode(dsserr.AlreadyExists, "Operation %s already exists", operation.ID.String())
	case old != nil && !operation.Version.Matches(old.Version):
		// The user wants to update an Operation but the version doesn't match.
		return nil, nil, stacktrace.NewErrorWithCode(dsserr.VersionMismatch, "Old version")
	case old != nil && old.Owner != operation.Owner:
		return nil, nil, stacktrace.NewErrorWithCode(dsserr.PermissionDenied, "Operation is owned by different client")
	}

	// Validate and perhaps correct StartTime and EndTime.
	if err := operation.ValidateTimeRange(); err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error validating time range")
	}

	// TODO(tvoss): Investigate whether we can fold the check for OVNs into the
	// the upsert query by means of a CTE and a coalescing condition testing
	// whether all affected OVNs are matched.
	switch operation.State {
	case scdmodels.OperationStateAccepted, scdmodels.OperationStateActivated:
		operations, err := s.searchOperations(ctx, s.q, &dssmodels.Volume4D{
			StartTime: operation.StartTime,
			EndTime:   operation.EndTime,
			SpatialVolume: &dssmodels.Volume3D{
				AltitudeHi: operation.AltitudeUpper,
				AltitudeLo: operation.AltitudeLower,
				Footprint: dssmodels.GeometryFunc(func() (s2.CellUnion, error) {
					return operation.Cells, nil
				}),
			},
		})
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "Error searching Operations")
		}

		keyIdx := map[scdmodels.OVN]struct{}{}
		for _, ovn := range key {
			keyIdx[ovn] = struct{}{}
		}

		for _, op := range operations {
			if _, match := keyIdx[op.OVN]; !match {
				return nil, nil, stacktrace.Propagate(scderr.ErrMissingOVNs, "Missing OVN for Operation %s", op.ID)
			}
			delete(keyIdx, op.OVN)
		}
	default:
		// Do not check the OVNs for any other operation states.
	}

	area, subscribers, err := s.pushOperation(ctx, s.q, operation)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "Error pushing Operation")
	}

	return area, subscribers, nil
}

func (s *repo) searchOperations(ctx context.Context, q dsssql.Queryable, v4d *dssmodels.Volume4D) ([]*scdmodels.Operation, error) {
	var (
		operationsIntersectingVolumeQuery = fmt.Sprintf(`
			SELECT
				%s
			FROM
				scd_operations
			JOIN
				(SELECT DISTINCT
					scd_cells_operations.operation_id
				FROM
					scd_cells_operations
				WHERE
					scd_cells_operations.cell_id = ANY($1)
				)
			AS
				unique_operations
			ON
				scd_operations.id = unique_operations.operation_id
			WHERE
				COALESCE(scd_operations.altitude_upper >= $2, true)
			AND
				COALESCE(scd_operations.altitude_lower <= $3, true)
			AND
				COALESCE(scd_operations.ends_at >= $4, true)
			AND
				COALESCE(scd_operations.starts_at <= $5, true)`, operationFieldsWithPrefix)
	)

	if v4d.SpatialVolume == nil || v4d.SpatialVolume.Footprint == nil {
		return nil, stacktrace.NewErrorWithCode(dsserr.BadRequest, "Missing geospatial footprint for query")
	}
	cells, err := v4d.SpatialVolume.Footprint.CalculateCovering()
	if err != nil {
		return nil, stacktrace.PropagateWithCode(err, dsserr.BadRequest, "Failed to calculate footprint covering")
	}
	if len(cells) == 0 {
		return nil, stacktrace.NewErrorWithCode(dsserr.BadRequest, "Missing cell IDs for query")
	}

	cids := make([]int64, len(cells))
	for i, cid := range cells {
		cids[i] = int64(cid)
	}

	result, err := s.fetchOperations(
		ctx, q, operationsIntersectingVolumeQuery,
		pq.Array(cids),
		v4d.SpatialVolume.AltitudeLo,
		v4d.SpatialVolume.AltitudeHi,
		v4d.StartTime,
		v4d.EndTime,
	)
	if err != nil {
		return nil, stacktrace.Propagate(err, "Error fetching Operations")
	}

	return result, nil
}

// SearchOperations returns operations within the 4D volume from CockroachDB
func (s *repo) SearchOperations(ctx context.Context, v4d *dssmodels.Volume4D) ([]*scdmodels.Operation, error) {
	result, err := s.searchOperations(ctx, s.q, v4d)
	if err != nil {
		return nil, err // No need to Propagate this error as this stack layer does not add useful information
	}

	return result, nil
}
