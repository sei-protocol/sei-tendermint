package indexer_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/adlio/schema"
	"github.com/ory/dockertest"
	"github.com/ory/dockertest/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	dbm "github.com/tendermint/tm-db"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/internal/eventbus"
	"github.com/tendermint/tendermint/internal/state/indexer"
	"github.com/tendermint/tendermint/internal/state/indexer/sink/kv"
	"github.com/tendermint/tendermint/internal/state/indexer/sink/psql"
	tmlog "github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/types"

	// Register the Postgre database driver.
	_ "github.com/lib/pq"
)

var psqldb *sql.DB
var resource *dockertest.Resource
var pSink indexer.EventSink

var (
	user     = "postgres"
	password = "secret"
	port     = "5432"
	dsn      = "postgres://%s:%s@localhost:%s/%s?sslmode=disable"
	dbName   = "postgres"
)

func TestIndexerServiceIndexesBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := tmlog.NewNopLogger()
	// event bus
	eventBus := eventbus.NewDefault(logger)
	err := eventBus.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(eventBus.Wait)

	assert.False(t, indexer.KVSinkEnabled([]indexer.EventSink{}))
	assert.False(t, indexer.IndexingEnabled([]indexer.EventSink{}))

	// event sink setup
	pool := setupDB(t)

	store := dbm.NewMemDB()
	eventSinks := []indexer.EventSink{kv.NewEventSink(store), pSink}
	assert.True(t, indexer.KVSinkEnabled(eventSinks))
	assert.True(t, indexer.IndexingEnabled(eventSinks))

	service := indexer.NewService(indexer.ServiceArgs{
		Logger:   logger,
		Sinks:    eventSinks,
		EventBus: eventBus,
	})
	require.NoError(t, service.Start(ctx))
	t.Cleanup(service.Wait)

	// publish block with txs
	err = eventBus.PublishEventNewBlockHeader(types.EventDataNewBlockHeader{
		Header: types.Header{Height: 1},
		NumTxs: int64(2),
	})
	require.NoError(t, err)
	txResult1 := &abci.TxResult{
		Height: 1,
		Index:  uint32(0),
		Tx:     types.Tx("foo"),
		Result: abci.ExecTxResult{Code: 0},
	}
	err = eventBus.PublishEventTx(types.EventDataTx{TxResult: *txResult1})
	require.NoError(t, err)
	txResult2 := &abci.TxResult{
		Height: 1,
		Index:  uint32(1),
		Tx:     types.Tx("bar"),
		Result: abci.ExecTxResult{Code: 0},
	}
	err = eventBus.PublishEventTx(types.EventDataTx{TxResult: *txResult2})
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	res, err := eventSinks[0].GetTxByHash(types.Tx("foo").Hash())
	require.NoError(t, err)
	require.Equal(t, txResult1, res)

	ok, err := eventSinks[0].HasBlock(1)
	require.NoError(t, err)
	require.True(t, ok)

	res, err = eventSinks[0].GetTxByHash(types.Tx("bar").Hash())
	require.NoError(t, err)
	require.Equal(t, txResult2, res)

	assert.Nil(t, teardown(t, pool))
}

func readSchema() ([]*schema.Migration, error) {
	filename := "./sink/psql/schema.sql"
	contents, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read sql file from '%s': %w", filename, err)
	}

	mg := &schema.Migration{}
	mg.ID = time.Now().Local().String() + " db schema"
	mg.Script = string(contents)
	return append([]*schema.Migration{}, mg), nil
}

func resetDB(t *testing.T) {
	q := "DROP TABLE IF EXISTS block_events,tx_events,tx_results"
	_, err := psqldb.Exec(q)
	assert.NoError(t, err)

	q = "DROP TYPE IF EXISTS block_event_type"
	_, err = psqldb.Exec(q)
	assert.NoError(t, err)
}

func setupDB(t *testing.T) *dockertest.Pool {
	t.Helper()
	pool, err := dockertest.NewPool(os.Getenv("DOCKER_URL"))
	assert.NoError(t, err)
	if _, err := pool.Client.Info(); err != nil {
		t.Skipf("WARNING: Docker is not available: %v [skipping this test]", err)
	}

	resource, err = pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "13",
		Env: []string{
			"POSTGRES_USER=" + user,
			"POSTGRES_PASSWORD=" + password,
			"POSTGRES_DB=" + dbName,
			"listen_addresses = '*'",
		},
		ExposedPorts: []string{fmt.Sprintf("%s/tcp", port)}, // Use the default PostgreSQL port inside container
	}, func(config *docker.HostConfig) {
		// set AutoRemove to true so that stopped container goes away by itself
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{
			Name: "no",
		}
	})

	assert.NoError(t, err)

	// Set the container to expire in a minute to avoid orphaned containers
	// hanging around
	_ = resource.Expire(60)

	conn := fmt.Sprintf(dsn, user, password, resource.GetPort(fmt.Sprintf("%s/tcp", port)), dbName)

	assert.NoError(t, pool.Retry(func() error {
		sink, err := psql.NewEventSink(conn, "test-chainID")
		if err != nil {
			return err
		}

		pSink = sink
		psqldb = sink.DB()
		return psqldb.Ping()
	}))

	resetDB(t)

	sm, err := readSchema()
	assert.NoError(t, err)

	migrator := schema.NewMigrator()
	err = migrator.Apply(psqldb, sm)
	assert.NoError(t, err)

	return pool
}

func teardown(t *testing.T, pool *dockertest.Pool) error {
	t.Helper()
	// When you're done, kill and remove the container
	assert.Nil(t, pool.Purge(resource))
	return psqldb.Close()
}
