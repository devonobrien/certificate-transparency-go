// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// ct_hammer is a stress/load test for a CT log.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/certificate-transparency-go/fixchain/ratelimiter"
	"github.com/google/certificate-transparency-go/trillian/ctfe"
	"github.com/google/certificate-transparency-go/trillian/ctfe/configpb"
	"github.com/google/certificate-transparency-go/trillian/integration"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/monitoring/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	ct "github.com/google/certificate-transparency-go"
)

var (
	banner              = flag.Bool("banner", true, "Display intro")
	httpServers         = flag.String("ct_http_servers", "localhost:8092", "Comma-separated list of (assumed interchangeable) servers, each as address:port")
	testDir             = flag.String("testdata_dir", "testdata", "Name of directory with test data")
	leafNotAfter        = flag.String("leaf_not_after", "", "Not-After date to use for leaf certs, RFC3339/ISO-8601 format (e.g. 2017-11-26T12:29:19Z)")
	metricsEndpoint     = flag.String("metrics_endpoint", "", "Endpoint for serving metrics; if left empty, metrics will not be exposed")
	seed                = flag.Int64("seed", -1, "Seed for random number generation")
	logConfig           = flag.String("log_config", "", "File holding log config in JSON")
	mmd                 = flag.Duration("mmd", 2*time.Minute, "Default MMD for logs")
	operations          = flag.Uint64("operations", ^uint64(0), "Number of operations to perform")
	minGetEntries       = flag.Int("min_get_entries", 1, "Minimum get-entries request size")
	maxGetEntries       = flag.Int("max_get_entries", 500, "Maximum get-entries request size")
	oversizedGetEntries = flag.Bool("oversized_get_entries", false, "Whether get-entries requests can go beyond log size")
	maxParallelChains   = flag.Int("max_parallel_chains", 2, "Maximum number of chains to add in parallel (will always add at least 1 chain)")
	limit               = flag.Int("rate_limit", 0, "Maximum rate of requests to an individual log; 0 for no rate limit")
	ignoreErrors        = flag.Bool("ignore_errors", false, "Whether to ignore errors and retry the operation")
	maxRetry            = flag.Duration("max_retry", 60*time.Second, "How long to keep retrying when ignore_errors is set")
)
var (
	addChainBias          = flag.Int("add_chain", 20, "Bias for add-chain operations")
	addPreChainBias       = flag.Int("add_pre_chain", 20, "Bias for add-pre-chain operations")
	getSTHBias            = flag.Int("get_sth", 2, "Bias for get-sth operations")
	getSTHConsistencyBias = flag.Int("get_sth_consistency", 2, "Bias for get-sth-consistency operations")
	getProofByHashBias    = flag.Int("get_proof_by_hash", 2, "Bias for get-proof-by-hash operations")
	getEntriesBias        = flag.Int("get_entries", 2, "Bias for get-entries operations")
	getRootsBias          = flag.Int("get_roots", 1, "Bias for get-roots operations")
	getEntryAndProofBias  = flag.Int("get_entry_and_proof", 0, "Bias for get-entry-and-proof operations")
	invalidChance         = flag.Int("invalid_chance", 10, "Chance of generating an invalid operation, as the N in 1-in-N (0 for never)")
)

func newLimiter(rate int) integration.Limiter {
	if rate <= 0 {
		return nil
	}
	return ratelimiter.NewLimiter(rate)
}

func main() {
	flag.Parse()
	if *logConfig == "" {
		glog.Exit("Test aborted as no log config provided (via --log_config)")
	}
	if *seed == -1 {
		*seed = time.Now().UTC().UnixNano() & 0xFFFFFFFF
	}
	fmt.Printf("Today's test has been brought to you by the letters C and T and the number %#x\n", *seed)
	rand.Seed(*seed)

	cfg, err := ctfe.LogConfigFromFile(*logConfig)
	if err != nil {
		glog.Exitf("Failed to read log config: %v", err)
	}

	var leafChain []ct.ASN1Cert
	var signer crypto.Signer
	if *testDir != "" {
		// Retrieve the test data.
		leafChain, err = integration.GetChain(*testDir, "leaf01.chain")
		if err != nil {
			glog.Exitf("failed to load certificate: %v", err)
		}
		signer, err = integration.MakeSigner(*testDir)
		if err != nil {
			glog.Exitf("failed to retrieve signer for re-signing: %v", err)
		}
	} else {
		glog.Warningf("Warning: add-[pre-]chain operations disabled as no cert generation method available")
		*addChainBias = 0
		*addPreChainBias = 0
	}

	var notAfterOverride time.Time
	if *leafNotAfter != "" {
		notAfterOverride, err = time.Parse(time.RFC3339, *leafNotAfter)
		if err != nil {
			glog.Exitf("Failed to parse leaf notAfter: %v", err)
		}
	}

	bias := integration.HammerBias{
		Bias: map[ctfe.EntrypointName]int{
			ctfe.AddChainName:          *addChainBias,
			ctfe.AddPreChainName:       *addPreChainBias,
			ctfe.GetSTHName:            *getSTHBias,
			ctfe.GetSTHConsistencyName: *getSTHConsistencyBias,
			ctfe.GetProofByHashName:    *getProofByHashBias,
			ctfe.GetEntriesName:        *getEntriesBias,
			ctfe.GetRootsName:          *getRootsBias,
			ctfe.GetEntryAndProofName:  *getEntryAndProofBias,
		},
		InvalidChance: map[ctfe.EntrypointName]int{
			ctfe.AddChainName:          *invalidChance,
			ctfe.AddPreChainName:       *invalidChance,
			ctfe.GetSTHName:            0,
			ctfe.GetSTHConsistencyName: *invalidChance,
			ctfe.GetProofByHashName:    *invalidChance,
			ctfe.GetEntriesName:        *invalidChance,
			ctfe.GetRootsName:          0,
			ctfe.GetEntryAndProofName:  0,
		},
	}

	var mf monitoring.MetricFactory
	if *metricsEndpoint != "" {
		mf = prometheus.MetricFactory{}
		http.Handle("/metrics", promhttp.Handler())
		server := http.Server{Addr: *metricsEndpoint, Handler: nil}
		glog.Infof("Serving metrics at %v", *metricsEndpoint)
		go func() {
			err := server.ListenAndServe()
			glog.Warningf("Metrics server exited: %v", err)
		}()
	} else {
		mf = monitoring.InertMetricFactory{}
	}

	if *banner {
		fmt.Print("\n\nStop")
		for i := 0; i < 8; i++ {
			time.Sleep(100 * time.Millisecond)
			fmt.Print(".")
		}
		mc := "H4sIAAAAAAAA/4xVPbLzMAjsv1OkU8FI9LqDOAUFDUNBxe2/QXYSS/HLe5SeXZYfsf73+D1KB8D2B2RxZpGw8gcsSoQYeH1ya0fof1BpnhpuUR+P8ijorESq8Yto6WYWqsrMGh4qSkdI/YFZWu8d3AAAkklEHBGTNAYxbpKltWRgRzQ3A3CImDIjVSVCicThbLK0VjsiAGAGIIKbmUcIq/KkqYo4BNZDqtgZMAPNPSJCRISZZ36d5OiTUbqJZAOYIoCHUreImJsCPMobQ20SqjBbLWWbBGRREhHQU2MMUu9TwB12cC7X3SNrs1yPKvv5gD4yn+kzshOfMg69fVknJNbdcsjuDvgNXWPmTXCuEnuvP4NdlSWymIQjfsFWzbERZ5sz730NpbvoOGMOzu7eeBUaW3w8r4z2iRuD4uY6W9wgZ96+YZvpHW7SabvlH7CviKWQyp81EL2zj7Fcbee7MpSuNHzj2z18LdAvAkAr8pr/3cGFUO+apa2n64TK3XouTBpEch2Rf8GnzajAFY438+SzgURfV7sXT+q1FNTJYdLF9WxJzFheAyNmXfKuiel5/mW2QqSx2umlQ+L2GpTPWZBu5tvpXW5/fy4xTYd2ly+vR052dZbjTIh0u4vzyRDF6kPzoRLRfhp2pqnr5wce5eAGP6onaRv8EYdl7gfd5zIId/gxYvr4pWW7KnbjoU6kRL62e25b44ZQz7Oaf4GrTovnqemNsyOdL40Dls11ocMPn29nYeUvmt3S1v8DAAD//wEAAP//TRo+KHEIAAA="
		mcData, _ := base64.StdEncoding.DecodeString(mc)
		b := bytes.NewReader(mcData)
		r, _ := gzip.NewReader(b)
		io.Copy(os.Stdout, r)
		r.Close()
		fmt.Print("\n\nHammer Time\n\n")
	}

	type result struct {
		prefix string
		err    error
	}
	results := make(chan result, len(cfg))
	var wg sync.WaitGroup
	for _, c := range cfg {
		wg.Add(1)
		pool, err := integration.NewRandomPool(*httpServers, c.PublicKey, c.Prefix)
		if err != nil {
			glog.Exitf("Failed to create client pool: %v", err)
		}

		mmd := *mmd
		// Note: Although the (usually lower than MMD) expected merge delay is not
		// a guarantee, it should be OK for testing.
		if emd := c.ExpectedMergeDelaySec; emd != 0 {
			mmd = time.Second * time.Duration(emd)
		}

		notAfter := notAfterOverride
		if notAfter.IsZero() {
			notAfter, err = notAfterForLog(c)
			if err != nil {
				glog.Exitf("Failed to determine notAfter for %s: %v", c.Prefix, err)
			}
		}
		generator, err := integration.NewSyntheticChainGenerator(leafChain, signer, notAfter)
		if err != nil {
			glog.Exitf("Failed to build chain generator: %v", err)
		}

		cfg := integration.HammerConfig{
			LogCfg:              c,
			MetricFactory:       mf,
			MMD:                 mmd,
			ChainGenerator:      generator,
			ClientPool:          pool,
			EPBias:              bias,
			MinGetEntries:       *minGetEntries,
			MaxGetEntries:       *maxGetEntries,
			OversizedGetEntries: *oversizedGetEntries,
			Operations:          *operations,
			Limiter:             newLimiter(*limit),
			MaxParallelChains:   *maxParallelChains,
			IgnoreErrors:        *ignoreErrors,
			MaxRetryDuration:    *maxRetry,
		}
		go func(cfg integration.HammerConfig) {
			defer wg.Done()
			err := integration.HammerCTLog(cfg)
			results <- result{prefix: cfg.LogCfg.Prefix, err: err}
		}(cfg)
	}
	wg.Wait()

	glog.Infof("completed tests on all %d logs:", len(cfg))
	close(results)
	errCount := 0
	for e := range results {
		if e.err != nil {
			errCount++
			glog.Errorf("  %s: failed with %v", e.prefix, e.err)
		}
	}
	if errCount > 0 {
		glog.Exitf("non-zero error count (%d), exiting", errCount)
	}
	glog.Info("  no errors; done")
}

// notAfterForLog returns a notAfter time to be used for certs submitted
// to the given log instance, allowing for any temporal shard configuration.
func notAfterForLog(c *configpb.LogConfig) (time.Time, error) {
	if c.NotAfterStart == nil && c.NotAfterLimit == nil {
		return time.Now().Add(24 * time.Hour), nil
	}

	if c.NotAfterStart != nil {
		start, err := ptypes.Timestamp(c.NotAfterStart)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to parse NotAfterStart: %v", err)
		}
		if c.NotAfterLimit == nil {
			return start.Add(24 * time.Hour), nil
		}

		limit, err := ptypes.Timestamp(c.NotAfterLimit)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to parse NotAfterLimit: %v", err)
		}
		return time.Unix(0, (limit.UnixNano()-start.UnixNano())/2+start.UnixNano()), nil
	}

	limit, err := ptypes.Timestamp(c.NotAfterLimit)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse NotAfterLimit: %v", err)
	}
	return limit.Add(-1 * time.Hour), nil
}
