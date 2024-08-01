// Copyright (C) 2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	avalancheWarp "github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/awm-relayer/signature-aggregator/api"
	"github.com/ava-labs/awm-relayer/signature-aggregator/metrics"
	testUtils "github.com/ava-labs/awm-relayer/tests/utils"
	"github.com/ava-labs/teleporter/tests/interfaces"
	"github.com/ava-labs/teleporter/tests/utils"
	"github.com/ethereum/go-ethereum/log"
	. "github.com/onsi/gomega"
)

func SignatureAggregatorAPI(network interfaces.LocalNetwork) {
	ctx := context.Background()

	subnetAInfo := network.GetPrimaryNetworkInfo()
	subnetBInfo, _ := utils.GetTwoSubnets(network)
	fundedAddress, fundedKey := network.GetFundedAccountInfo()

	log.Info("Sending teleporter message")
	receipt, _, _ := testUtils.SendBasicTeleporterMessage(
		ctx,
		subnetAInfo,
		subnetBInfo,
		fundedKey,
		fundedAddress,
	)
	warpMessage := getWarpMessageFromLog(ctx, receipt, subnetAInfo)

	signatureAggregatorConfig := testUtils.CreateDefaultSignatureAggregatorConfig(
		[]interfaces.SubnetTestInfo{subnetAInfo, subnetBInfo},
	)

	signatureAggregatorConfigPath := testUtils.WriteSignatureAggregatorConfig(
		signatureAggregatorConfig,
		testUtils.DefaultSignatureAggregatorCfgFname,
	)
	log.Info("Starting the signature aggregator", "configPath", signatureAggregatorConfigPath)
	signatureAggregatorCancel := testUtils.BuildAndRunSignatureAggregatorExecutable(ctx, signatureAggregatorConfigPath)
	defer signatureAggregatorCancel()

	// Sleep for some time to make sure signature aggregator has started up and subscribed.
	log.Info("Waiting for the signature aggregator to start up")
	time.Sleep(5 * time.Second)

	reqBody := api.AggregateSignaturesByRawMsgRequest{
		UnsignedMessage: "0x" + hex.EncodeToString(warpMessage.Bytes()),
	}

	client := http.Client{
		Timeout: 20 * time.Second,
	}

	requestURL := fmt.Sprintf("http://localhost:%d%s", signatureAggregatorConfig.APIPort, api.RawMessageAPIPath)

	// Send request to API
	{
		b, err := json.Marshal(reqBody)
		Expect(err).Should(BeNil())
		bodyReader := bytes.NewReader(b)

		req, err := http.NewRequest(http.MethodPost, requestURL, bodyReader)
		Expect(err).Should(BeNil())
		req.Header.Set("Content-Type", "application/json")

		res, err := client.Do(req)
		Expect(err).Should(BeNil())
		Expect(res.Status).Should(Equal("200 OK"))
		Expect(res.Header.Get("Content-Type")).Should(Equal("application/json"))

		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		Expect(err).Should(BeNil())

		var response api.AggregateSignaturesResponse
		err = json.Unmarshal(body, &response)
		Expect(err).Should(BeNil())

		decodedMessage, err := hex.DecodeString(response.SignedMessage)
		Expect(err).Should(BeNil())

		signedMessage, err := avalancheWarp.ParseMessage(decodedMessage)
		Expect(err).Should(BeNil())
		Expect(signedMessage.ID()).Should(Equal(warpMessage.ID()))
	}

	// Check metrics
	metricsSample := sampleMetrics(signatureAggregatorConfig.MetricsPort)
	Expect(
		metricsSample[metrics.Opts.AggregateSignaturesRequestCount.Name],
	).Should(BeNumerically("==", 1))
	Expect(
		metricsSample[metrics.Opts.AggregateSignaturesLatencyMS.Name],
	).Should(BeNumerically(">", 0))
	Expect(
		metricsSample[metrics.Opts.ValidatorFailures.Name],
	).Should(BeNumerically("<", 11))
}

// returns a map of metric names to metric samples
func sampleMetrics(port uint16) map[string]uint64 {
	resp, err := http.Get(
		fmt.Sprintf("http://localhost:%d/metrics", port),
	)
	Expect(err).Should(BeNil())

	body, err := io.ReadAll(resp.Body)
	Expect(err).Should(BeNil())
	defer resp.Body.Close()

	var samples = make(map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := scanner.Text()
		for _, metricName := range []string{
			metrics.Opts.AggregateSignaturesLatencyMS.Name,
			metrics.Opts.AggregateSignaturesRequestCount.Name,
			metrics.Opts.ValidatorFailures.Name,
		} {
			if strings.HasPrefix(
				line,
				"U__signature_2d_aggregator_"+metricName,
			) {
				log.Debug("Found metric line", "line", line)
				parts := strings.Fields(line)

				// Fetch the metric count from the last field of the line
				value, err := strconv.ParseUint(parts[len(parts)-1], 10, 64)
				if err != nil {
					log.Warn("failed to parse value from metric line")
					continue
				}
				log.Debug("parsed metric", "name", metricName, "value", value)

				samples[metricName] = value
			} else {
				log.Debug("Ignoring non-metric line", "line", line)
			}
		}
	}
	return samples
}
