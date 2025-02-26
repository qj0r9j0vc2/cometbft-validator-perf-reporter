package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	Timeout    = 30
	TargetHost = "127.0.0.1:26657"
)

// NodeStatus wraps whether we successfully connected and the relevant block info.
type NodeStatus struct {
	Open     bool
	Moniker  string
	Earliest BlockPoint
	Latest   BlockPoint
	Network  string
	Err      error // If we had an error, store it here for logging or debugging
}

type BlockPoint struct {
	Height string
	Time   time.Time
}

// StatusType represents the signature status of a block.
type StatusType int

const (
	StatusUnknown            = -1
	StatusMissed  StatusType = iota
	StatusSigned
	StatusProposed
)

func main() {
	blocksToCheck := flag.Int("blocks", 10, "Number of blocks to check for validator signatures")
	lastHeight := flag.Int64("last", 0, "Block height to check until")
	validator := flag.String("validator", "", "Target validator consensus address in hex (uppercase)")
	targetRPC := flag.String("target", "", "target RPC to query")
	logLevel := flag.String("log-level", "info", "log level")
	flag.Parse()
	if *validator == "" {
		log.Fatal("validator flag is required")
	}
	if *targetRPC == "" {
		log.Fatal("target RPC flag is required")
	}

	l, err := log.ParseLevel(*logLevel)
	if err != nil {
		l = log.InfoLevel
	}
	log.SetLevel(l)

	maxWorkers := runtime.GOMAXPROCS(0)
	log.Infof("Setting validator: %s, blocksToCheck: %d, semaphore: %d, targetRPC: %s, log level: %s", *validator, *blocksToCheck, maxWorkers, *targetRPC, *logLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &http.Client{
		Timeout: Timeout * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: maxWorkers,
		},
	}

	var validHost string = TargetHost

	statusRes, err := client.Get(addPrefix(fmt.Sprintf("%s/status", validHost)))
	if err != nil {
		log.Fatal(err)
	}
	defer statusRes.Body.Close()
	statusBody, err := io.ReadAll(statusRes.Body)
	if err != nil {
		log.Fatal(err)
	}
	var statusResp CometBFTStatusResult
	if err = json.Unmarshal(statusBody, &statusResp); err != nil {
		log.Fatal(err)
	}

	var latestHeight int64
	if *lastHeight == 0 {
		latestHeight, err = strconv.ParseInt(statusResp.Result.SyncInfo.LatestBlockHeight, 10, 64)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		latestHeight = *lastHeight
	}
	startHeight := latestHeight - int64(*blocksToCheck) + 1
	if startHeight < 1 {
		startHeight = 1
	}
	log.Infof("Checking validator signature stats from block %d to %d on host %s", startHeight, latestHeight, validHost)

	rawFile := fmt.Sprintf("%s_%d-%d-blocks.txt", *validator, startHeight, startHeight+int64(*blocksToCheck))
	res, err := checkValidatorSignatureStats(ctx, rawFile, client, validHost, startHeight, *blocksToCheck, *validator, maxWorkers)
	if err != nil {
		log.Error(err)
	}
	log.Infof(res)
	err = os.WriteFile(fmt.Sprintf("%s_%d-%d.txt", *validator, startHeight, startHeight+int64(*blocksToCheck)), []byte(fmt.Sprintf("%s", res)), 0644)
	if err != nil {
		log.Fatal(err)
	}
}

func logProgress(ctx context.Context, rpcMap map[string]*NodeStatus, mtx *sync.Mutex) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mtx.Lock()
			count := len(rpcMap)
			mtx.Unlock()
			log.Infof("Discovered %d nodes so far...", count)
		}
	}
}

func setErrorStatus(rpcMap map[string]*NodeStatus, mtx *sync.Mutex, host string, err error) {
	mtx.Lock()
	defer mtx.Unlock()
	if ns, ok := rpcMap[host]; ok && ns != nil {
		ns.Err = err
	}
}

func addPrefix(host string) string {
	if strings.HasPrefix(host, "http") {
		return host
	}
	return fmt.Sprintf("http://%s", host)
}

type stat struct {
	proposed uint64
	signed   uint64
	missed   uint64
	unknown  uint64
}

func checkValidatorSignatureStats(ctx context.Context, filename string, client *http.Client, host string, startHeight int64, blocksToCheck int, targetValidator string, semSize int) (string, error) {
	var (
		mu     = new(sync.Mutex)
		stats  = make(map[string]stat)
		blocks = make(map[int64]map[string]StatusType)
	)

	sem := semaphore.NewWeighted(int64(semSize))

	// Start progress ticker goroutine
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	getSize := func(totalSize int) int {
		target := 100
		divider := 10
		for {
			if totalSize/divider < target {
				return divider
			} else {
				divider = divider + 10
			}
		}
	}
	div := getSize(blocksToCheck)

	go func() {
		err := func() error {
			for {
				select {
				case <-ticker.C:
					clearScreen()
					fmt.Printf(updateGauge(blocksToCheck/div, blocksToCheck/div, len(blocks)/div))
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}()
		if err != nil {
			log.Error(err)
		}
	}()

	lastHeight := startHeight + int64(blocksToCheck)

	validators, err := getValidatorSet(client, ctx, host, startHeight)
	if err != nil {
		log.Fatal(err)
	}

	// Process blocks concurrently
	for height := startHeight; height < lastHeight; height++ {

		if _, ok := blocks[height]; !ok {
			mu.Lock()
			blocks[height] = make(map[string]StatusType)
			mu.Unlock()
		}

		if err = sem.Acquire(ctx, 1); err != nil {
			log.Printf("Failed to acquire semaphore: %v", err)
			break
		}

		go func(h int64) {
			defer sem.Release(1)

			url := addPrefix(fmt.Sprintf("%s/block?height=%d", host, h))
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				log.Errorf("Error creating request for block %d: %v", h, err)
				return
			}

			var res *http.Response
			maxRetries := 5
			for attempt := 0; attempt < maxRetries; attempt++ {
				res, err = client.Do(req)
				if err == nil {
					break
				}
				log.Errorf("Error fetching block %d (attempt %d/%d): %v", h, attempt+1, maxRetries, err)
				time.Sleep(1 * time.Second)
			}
			if err != nil {
				log.Errorf("Failed to fetch block %d after %d attempts: %v", h, maxRetries, err)
				return
			}
			defer res.Body.Close()

			body, err := io.ReadAll(res.Body)
			if err != nil {
				log.Errorf("Error reading block %d: %v", h, err)
				return
			}

			var rb CometBFTBlockResult
			if err := json.Unmarshal(body, &rb); err != nil {
				log.Errorf("Error unmarshaling block %d: %v", h, err)
				return
			}

			var status StatusType
			for i := 0; i < len(validators); i++ {
				status = determineStatus(rb, validators[i], h, body)
				mu.Lock()
				st := stats[validators[i]]
				switch status {
				case StatusProposed:
					st.proposed++
				case StatusSigned:
					st.signed++
				case StatusMissed:
					st.missed++
				case StatusUnknown:
					st.unknown++
				}
				stats[validators[i]] = st
				blocks[h][validators[i]] = status
				mu.Unlock()
			}

			log.Debugf("completed block %d", h)
			return
		}(height)
	}

	// Acquire all of the tokens to wait for any remaining workers to finish.
	//
	// If you are already waiting for the workers by some other means (such as an
	// errgroup.Group), you can omit this final Acquire call.
	if err = sem.Acquire(ctx, int64(semSize)); err != nil {
		log.Printf("Failed to acquire semaphore: %v", err)
	} else {
		log.Infof("complete to fetch data. processing...")
	}

	// Build raw result using strings.Builder
	file, err := os.Create(filename)
	if err != nil {
		log.Fatalf("Cannot create output file: %v", err)
	}
	defer file.Close()

	w := bufio.NewWriter(file)
	if _, err = w.WriteString(
		"0: Proposed, 1: Signed, 2: Missed, 3: Unknown\n\n"); err != nil {
		log.Fatalf("Cannot write output file: %v", err)
	}

	if _, err = w.WriteString(
		fmt.Sprintf("| %10s |", "Height")); err != nil {
		log.Fatalf("Cannot write output file: %v", err)
	}
	for vIdx := 0; vIdx < len(validators); vIdx++ {
		if _, err = w.WriteString(
			fmt.Sprintf("| %40s |", validators[vIdx])); err != nil {
			log.Fatalf("Cannot write output file: %v", err)
		}
	}

	if _, err = w.WriteString("\n"); err != nil {
		log.Fatalf("Cannot write output file: %v", err)
	}

	const chunkSize = 10000

	for i := startHeight; i < startHeight+int64(blocksToCheck); i++ {
		if _, err = w.WriteString(
			fmt.Sprintf("| %10d |", i)); err != nil {
			log.Fatalf("Cannot write output file: %v", err)
		}
		for vIdx := 0; vIdx < len(validators); vIdx++ {
			status, ok := blocks[i][validators[vIdx]]
			if !ok {
				status = StatusUnknown
			}
			if _, err = w.WriteString(fmt.Sprintf("| %40d |", status)); err != nil {
				return "", fmt.Errorf("failed to record at block %d: %w", i, err)
			}

			if (i-startHeight+1)%chunkSize == 0 {
				if err = w.Flush(); err != nil {
					return "", fmt.Errorf("failed to flush chunk: %w", err)
				}
			}
		}

		if _, err = w.WriteString(fmt.Sprintf("\n")); err != nil {
			return "", fmt.Errorf("failed to record at block %d: %w", i, err)
		}
	}

	if _, err = w.WriteString(fmt.Sprintf("==================== %10s ====================\n", "Result")); err != nil {
		log.Errorf("failed to write results: %v", err)
	}

	var rank []string
	for key := range stats {
		rank = append(rank, key)
	}

	sort.Slice(rank, func(i, j int) bool {
		si, sj := stats[rank[i]], stats[rank[j]]
		if si.missed == sj.missed {
			return si.proposed > sj.proposed
		}
		return si.missed < sj.missed
	})

	for _, valAddr := range rank {
		if _, err = w.WriteString(fmt.Sprintf("%s: Proposed: %d, Signed: %d, Missed: %d, Unknown: %d\n", valAddr, stats[valAddr].proposed, stats[valAddr].signed, stats[valAddr].missed, stats[valAddr].unknown)); err != nil {
			log.Errorf("Failed to write signing stats %s: %v", valAddr, err)
			continue
		}
	}

	if err := w.Flush(); err != nil {
		return "", fmt.Errorf("failed to flush: %w", err)
	}

	clearScreen()

	summary := fmt.Sprintf("Validator signature %s stats from block %d to %d: Proposed: %d, Signed: %d, Missed: %d, Unknown: %d",
		targetValidator, startHeight, startHeight+int64(blocksToCheck)-1, stats[targetValidator].proposed, stats[targetValidator].signed, stats[targetValidator].missed, stats[targetValidator].unknown)

	return summary, nil
}

// Example helper to determine status; refactor your current logic into this function.
func determineStatus(rb CometBFTBlockResult, targetValidator string, height int64, body []byte) StatusType {
	if rb.Result.Block == nil || rb.Result.Block.Header == nil || rb.Result.Block.LastCommit == nil {
		log.Errorf("Error processing block %d: %s", height, string(body))
		return StatusUnknown
	}
	if rb.Result.Block.Header.ProposerAddress == targetValidator {
		return StatusProposed
	}
	for _, sig := range rb.Result.Block.LastCommit.Signatures {
		if sig.ValidatorAddress == targetValidator {
			return StatusSigned
		}
	}
	return StatusMissed
}

func getValidatorSet(client *http.Client, ctx context.Context, host string, height int64) ([]string, error) {
	url := addPrefix(fmt.Sprintf("%s/validators?height=%d&per_page=10000", host, height))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "Error creating request for block %d", height)
	}

	var res *http.Response

	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		res, err = client.Do(req)
		if err == nil {
			break
		}
		log.Warningf("Error fetching validator at height: %d (attempt %d/%d): %v", height, attempt+1, maxRetries, err)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return nil, errors.Wrapf(err, "Error fetching validator at height: %d", height)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "Error reading block %d: %v", height, err)
	}

	var cometValRes CometBFTValidatorResult

	if err = json.Unmarshal(body, &cometValRes); err != nil {
		return nil, errors.Wrapf(err, "Error unmarshaling validator result %d", height)
	}

	var validators []string
	for _, val := range cometValRes.Result.Validators {
		validators = append(validators, val.Address)
	}

	sort.Strings(validators)
	return validators, nil
}

type CometBFTValidatorResult struct {
	Result  Validators `json:"result"`
	ID      any        `json:"id"`
	Jsonrpc string     `json:"jsonrpc"`
}

type Validators struct {
	Total       string             `json:"total"`
	Validators  []ValidatorElement `json:"validators"`
	Count       string             `json:"count"`
	BlockHeight string             `json:"block_height"`
}

type ValidatorElement struct {
	Address          string `json:"address"`
	ProposerPriority string `json:"proposer_priority"`
	PubKey           PubKey `json:"pub_key"`
	VotingPower      string `json:"voting_power"`
}

type PubKey struct {
	Type  Type   `json:"type"`
	Value string `json:"value"`
}

type Type string

const (
	TendermintPubKeyEd25519 Type = "tendermint/PubKeyEd25519"
)

type CometBFTStatusResult struct {
	Result  ResultStatus `json:"result"`
	ID      any          `json:"id"`
	Jsonrpc string       `json:"jsonrpc"`
}

type ResultStatus struct {
	NodeInfo      DefaultNodeInfo `json:"node_info"`
	SyncInfo      SyncInfo        `json:"sync_info"`
	ValidatorInfo ValidatorInfo   `json:"validator_info"`
}

type DefaultNodeInfo struct {
	ProtocolVersion ProtocolVersion      `json:"protocol_version"`
	DefaultNodeID   string               `json:"id"`
	ListenAddr      string               `json:"listen_addr"`
	Network         string               `json:"network"`
	Version         string               `json:"version"`
	Channels        HexBytes             `json:"channels"`
	Moniker         string               `json:"moniker"`
	Other           DefaultNodeInfoOther `json:"other"`
}

type ProtocolVersion struct {
	P2P   string `json:"p2p"`
	Block string `json:"block"`
	App   string `json:"app"`
}

type HexBytes string

type DefaultNodeInfoOther struct {
	TxIndex    string `json:"tx_index"`
	RPCAddress string `json:"rpc_address"`
}

type SyncInfo struct {
	LatestBlockHash     HexBytes  `json:"latest_block_hash"`
	LatestAppHash       HexBytes  `json:"latest_app_hash"`
	LatestBlockHeight   string    `json:"latest_block_height"`
	LatestBlockTime     time.Time `json:"latest_block_time"`
	EarliestBlockHash   HexBytes  `json:"earliest_block_hash"`
	EarliestAppHash     HexBytes  `json:"earliest_app_hash"`
	EarliestBlockHeight string    `json:"earliest_block_height"`
	EarliestBlockTime   time.Time `json:"earliest_block_time"`
	CatchingUp          bool      `json:"catching_up"`
}

type ValidatorInfo struct {
	Address     HexBytes `json:"address"`
	PubKey      any      `json:"pub_key"`
	VotingPower string   `json:"voting_power"`
}

// --- Types for /block response (validator signatures check) ---

type CometBFTBlockResult struct {
	Result  ResultBlock `json:"result"`
	ID      any         `json:"id"`
	Jsonrpc string      `json:"jsonrpc"`
}

type ResultBlock struct {
	BlockID json.RawMessage `json:"block_id"`
	Block   *Block          `json:"block"`
}

type Block struct {
	Header     *Header `json:"header"`
	LastCommit *Commit `json:"last_commit"`
}

type Header struct {
	Height          int64  `json:"height,string"`
	ProposerAddress string `json:"proposer_address"`
}

type Commit struct {
	Height     int64       `json:"height,string"`
	Round      int32       `json:"round"`
	Signatures []CommitSig `json:"signatures"`
}

type CommitSig struct {
	ValidatorAddress string    `json:"validator_address"`
	Timestamp        time.Time `json:"timestamp"`
	Signature        string    `json:"signature"`
}

func updateGauge(gaugeBlockSize, gaugeTotalSize, gaugeCurrent int) string {

	gaugeBlockSize = gaugeBlockSize - gaugeBlockSize%gaugeTotalSize
	until := gaugeCurrent * (gaugeBlockSize / gaugeTotalSize)

	var imoji = ""

	if until < (gaugeBlockSize / 3 * 1) {
		imoji = "🛠️"
	} else if until < (gaugeBlockSize / 3 * 2) {
		imoji = "🚧"
	} else if until <= (gaugeBlockSize / 3 * 3) {
		imoji = "🟢"
	}

	bar := fmt.Sprintf("%v%s", imoji, "[")

	for i := 0; i < until; i++ {
		bar += "="
	}

	for i := until; i < gaugeBlockSize; i++ {
		bar += "."
	}
	bar += "]"

	gaugeCurrent++
	return bar
}

var afterClearMsg string

func clearScreen() {
	// Adjust the command according to your OS if needed.
	c := exec.Command("clear")
	c.Stdout = os.Stdout
	c.Run()

	println(afterClearMsg)
}
