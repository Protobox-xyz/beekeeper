package smoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/ethersphere/bee/pkg/swarm"
	"github.com/ethersphere/beekeeper/pkg/bee"
	"github.com/ethersphere/beekeeper/pkg/bee/api"
	"github.com/ethersphere/beekeeper/pkg/beekeeper"
	"github.com/ethersphere/beekeeper/pkg/logging"
	"github.com/ethersphere/beekeeper/pkg/orchestration"
	"github.com/ethersphere/beekeeper/pkg/random"
)

// Options represents smoke test options
type Options struct {
	ContentSize   int64
	RndSeed       int64
	PostageAmount int64
	PostageDepth  uint64
	TxOnErrWait   time.Duration
	RxOnErrWait   time.Duration
	NodesSyncWait time.Duration
	Duration      time.Duration
	// load test params
	UploaderCount   int
	UploadGroups    []string
	DownloaderCount int
	DownloadGroups  []string
	GasPrice        string
	MaxUseBatch     time.Duration
}

// NewDefaultOptions returns new default options
func NewDefaultOptions() Options {
	return Options{
		ContentSize:   5000000,
		RndSeed:       time.Now().UnixNano(),
		PostageAmount: 1000000,
		PostageDepth:  20,
		TxOnErrWait:   10 * time.Second,
		RxOnErrWait:   10 * time.Second,
		NodesSyncWait: time.Minute,
		Duration:      12 * time.Hour,
		GasPrice:      "100000000000",
		MaxUseBatch:   time.Hour * 3,
	}
}

// compile check whether Check implements interface
var _ beekeeper.Action = (*Check)(nil)

// Check instance
type Check struct {
	metrics metrics
	logger  logging.Logger
}

// NewCheck returns new check
func NewCheck(logger logging.Logger) beekeeper.Action {
	return &Check{
		metrics: newMetrics("check_smoke"),
		logger:  logger,
	}
}

// Run creates file of specified size that is uploaded and downloaded.
func (c *Check) Run(ctx context.Context, cluster orchestration.Cluster, opts interface{}) error {
	o, ok := opts.(Options)
	if !ok {
		return fmt.Errorf("invalid options type")
	}

	c.logger.Info("random seed: ", o.RndSeed)
	c.logger.Info("content size: ", o.ContentSize)

	rnd := random.PseudoGenerator(o.RndSeed)

	clients, err := cluster.NodesClients(ctx)
	if err != nil {
		return err
	}

	time.Sleep(5 * time.Second) // Wait for the nodes to warmup.

	// The test will restart itself every 12 hours (default, if not specified diferrently in config),
	// this is in order to create more meaningful metrics, so that we can apply prometheus
	// functions to them.
	ctx, cancel := context.WithTimeout(ctx, o.Duration)
	defer cancel()

	test := &test{opt: o, ctx: ctx, clients: clients, logger: c.logger}

	for i := 0; true; i++ {
		select {
		case <-ctx.Done():
			return nil
		default:
			c.logger.Infof("starting iteration: #%d", i)
		}

		perm := rnd.Perm(cluster.Size())
		txIdx := perm[0]
		rxIdx := perm[1]

		// if the upload and download nodes are the same, try again for a different peer
		if txIdx == rxIdx {
			continue
		}

		nn := cluster.NodeNames()
		txName := nn[txIdx]
		rxName := nn[rxIdx]

		c.logger.Infof("uploader: %s", txName)
		c.logger.Infof("downloader: %s", rxName)

		var (
			txDuration time.Duration
			rxDuration time.Duration
			txData     []byte
			rxData     []byte
			address    swarm.Address
		)

		txData = make([]byte, o.ContentSize)
		if _, err := rand.Read(txData); err != nil {
			c.logger.Infof("unable to create random content: %v", err)
			continue
		}

		for retries := 3; txDuration == 0 && retries > 0; retries-- {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			c.metrics.UploadAttempts.Inc()

			address, txDuration, err = test.upload(txName, txData)
			if err != nil {
				c.metrics.UploadErrors.Inc()
				c.logger.Infof("upload failed: %v", err)
				c.logger.Infof("retrying in: %v", o.TxOnErrWait)
				time.Sleep(o.TxOnErrWait)
			}
		}

		if txDuration == 0 {
			continue
		}

		c.metrics.UploadDuration.Observe(txDuration.Seconds())

		time.Sleep(o.NodesSyncWait) // Wait for nodes to sync.

		for retries := 3; rxDuration == 0 && retries > 0; retries-- {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(o.RxOnErrWait):
			}

			c.metrics.DownloadAttempts.Inc()

			rxData, rxDuration, err = test.download(rxName, address)
			if err != nil {
				c.metrics.DownloadErrors.Inc()
				c.logger.Infof("download failed: %v", err)
				c.logger.Infof("retrying in: %v", o.RxOnErrWait)
				continue
			}

			if rxDuration == 0 {
				continue
			}

			if bytes.Equal(rxData, txData) {
				c.metrics.DownloadDuration.Observe(rxDuration.Seconds())
				break
			}

			c.logger.Info("uploaded data does not match downloaded data")

			c.metrics.DownloadMismatch.Inc()

			rxLen, txLen := len(rxData), len(txData)
			if rxLen != txLen {
				c.logger.Infof("length mismatch: download length %d; upload length %d", rxLen, txLen)
				if txLen < rxLen {
					c.logger.Info("length mismatch: rx length is bigger then tx length")
				}
				continue
			}

			var diff int
			for i := range txData {
				if txData[i] != rxData[i] {
					diff++
				}
			}
			c.logger.Infof("data mismatch: found %d different bytes, ~%.2f%%", diff, float64(diff)/float64(txLen)*100)
		}
	}

	return nil
}

type test struct {
	opt     Options
	ctx     context.Context
	clients map[string]*bee.Client
	logger  logging.Logger
}

func (t *test) uploadWithBatch(cName string, data []byte, batchID string) (swarm.Address, time.Duration, error) {
	client := t.clients[cName]
	t.logger.Infof("node %s: uploading data, batch id %s", cName, batchID)
	start := time.Now()
	addr, err := client.UploadBytes(t.ctx, data, api.UploadOptions{Pin: false, BatchID: batchID, Direct: true})
	if err != nil {
		return swarm.ZeroAddress, 0, fmt.Errorf("upload to the node %s: %w", cName, err)
	}
	txDuration := time.Since(start)
	t.logger.Infof("node %s: upload done in %s", cName, txDuration)

	return addr, txDuration, nil
}

func (t *test) upload(cName string, data []byte) (swarm.Address, time.Duration, error) {
	client := t.clients[cName]
	batchID, err := client.GetOrCreateBatch(t.ctx, t.opt.PostageAmount, t.opt.PostageDepth, t.opt.GasPrice, "smoke-test")
	if err != nil {
		return swarm.ZeroAddress, 0, fmt.Errorf("node %s: unable to create batch id: %w", cName, err)
	}
	t.logger.Infof("node %s: uploading data, batch id %s", cName, batchID)
	start := time.Now()
	addr, err := client.UploadBytes(t.ctx, data, api.UploadOptions{Pin: false, BatchID: batchID, Direct: false})
	if err != nil {
		return swarm.ZeroAddress, 0, fmt.Errorf("upload to the node %s: %w", cName, err)
	}
	txDuration := time.Since(start)
	t.logger.Infof("node %s: upload done in %s", cName, txDuration)

	return addr, txDuration, nil
}

func (t *test) download(cName string, addr swarm.Address) ([]byte, time.Duration, error) {
	client := t.clients[cName]
	t.logger.Infof("node %s: downloading address %s", cName, addr)
	start := time.Now()
	data, err := client.DownloadBytes(t.ctx, addr)
	if err != nil {
		return nil, 0, fmt.Errorf("download from node %s: %w", cName, err)
	}
	rxDuration := time.Since(start)
	t.logger.Infof("node %s: download done in %s", cName, rxDuration)

	return data, rxDuration, nil
}
