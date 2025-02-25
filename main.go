package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	Timeout    = 30
	Semaphore  = 100
	TargetHost = "88.99.211.241:26657"
	ChainId    = "injective-1"
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
	Statusmissed StatusType = iota
	StatusSigned
	StatusProposed
	StatusUnkown
)

func main() {
	blocksToCheck := flag.Int("blocks", 10, "Number of blocks to check for validator signatures")
	lastHeight := flag.Int64("last", 0, "Block height to check until")
	validator := flag.String("validator", "", "Target validator consensus address in hex (uppercase)")
	semaphore := flag.Int("semaphore", 0, "Semaphore pool size")
	targetRPC := flag.String("target", "", "target RPC to query")
	flag.Parse()
	if *validator == "" {
		log.Fatal("validator flag is required")
	}
	if *semaphore == 0 {
		*semaphore = Semaphore
	}
	if *targetRPC == "" {
		log.Fatal("target RPC flag is required")
	}

	log.SetLevel(log.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &http.Client{
		Timeout: Timeout * time.Second,
	}

	var validHost string = TargetHost
	//for host, status := range rpcMap {
	//	if status.Open && status.Network == ChainId {
	//		log.Infof("%40s, moniker=%-40s start: %10s/%24s, latest: %10s/%24s",
	//			host, status.Moniker,
	//			status.Earliest.Height, status.Earliest.Time.Format(time.RFC3339),
	//			status.Latest.Height, status.Latest.Time.Format(time.RFC3339),
	//		)
	//		validHost = host // select the first valid host
	//		break
	//	} else if status.Open && status.Network != ChainId {
	//		log.Warningf("Network mismatch for host=%s, got=%s, want=%s",
	//			host, status.Network, ChainId)
	//	}
	//}
	//if validHost == "" {
	//	log.Fatal("No valid host found for block query")
	//}

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

	blockSem := make(chan struct{}, *semaphore)
	raw, res := checkValidatorSignatureStats(ctx, client, validHost, startHeight, *blocksToCheck, *validator, blockSem)
	log.Infof(res)
	err = os.WriteFile(fmt.Sprintf("%s_%d-%d.txt", *validator, startHeight, startHeight+int64(*blocksToCheck)), []byte(fmt.Sprintf("%s\n%s", res, raw)), 0644)
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

func checkValidatorSignatureStats(ctx context.Context, client *http.Client, host string, startHeight int64, blocksToCheck int, targetValidator string, sem chan struct{}) (string, string) {
	var wg sync.WaitGroup
	var mtx sync.Mutex
	stats := struct {
		proposed uint64
		signed   uint64
		missed   uint64
		unknown  uint64
	}{}
	defer ctx.Done()

	var blocks = map[int64]StatusType{}

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
		t := time.NewTicker(1 * time.Second)

		for {
			clearScreen()
			fmt.Printf(updateGauge(blocksToCheck/div, blocksToCheck/div, len(blocks)/div))

			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	for h := startHeight; h < startHeight+int64(blocksToCheck); h++ {
		wg.Add(1)
		go func(height int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			url := addPrefix(fmt.Sprintf("%s/block?height=%d", host, height))
			res, err := client.Get(url)
			if err != nil {
				log.Errorf("Error fetching block %d: %v", height, err)
				return
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				log.Errorf("Error reading block %d: %v", height, err)
				return
			}
			var rb CometBFTBlockResult
			if err := json.Unmarshal(body, &rb); err != nil {
				log.Errorf("Error unmarshaling block %d: %v", height, err)
				return
			}

			var status StatusType
			if rb.Result.Block == nil || rb.Result.Block.Header == nil || rb.Result.Block.LastCommit == nil {
				log.Errorf("Error unmarshaling block %d: %s", height, string(body))
				status = StatusUnkown
			} else if rb.Result.Block.Header.ProposerAddress == targetValidator {
				status = StatusProposed
			} else {
				found := false
				for _, sig := range rb.Result.Block.LastCommit.Signatures {
					if sig.ValidatorAddress == targetValidator {
						found = true
						break
					}
				}
				if found {
					status = StatusSigned
				} else {
					status = Statusmissed
				}
			}
			mtx.Lock()
			switch status {
			case StatusProposed:
				stats.proposed++
				blocks[height] = StatusProposed
			case StatusSigned:
				stats.signed++
				blocks[height] = StatusSigned
			case Statusmissed:
				stats.missed++
				blocks[height] = Statusmissed
			case StatusUnkown:
				stats.unknown++
				blocks[height] = StatusUnkown
			}

			mtx.Unlock()
		}(h)
	}

	wg.Wait()
	var rawRes = fmt.Sprintf("0: Proposed, 1: Signed, 2: Missed, 3: Unknown\n")
	for i := startHeight; i < startHeight+int64(blocksToCheck); i++ {
		rawRes = rawRes + fmt.Sprintf("%d: %d\n", i, blocks[i])
	}
	clearScreen()
	return rawRes, fmt.Sprintf("Validator signature %s stats from block %d to %d: Proposed: %d, Signed: %d, Missed: %d, Unknown: %d",
		targetValidator, startHeight, startHeight+int64(blocksToCheck)-1, stats.proposed, stats.signed, stats.missed, stats.unknown)
}

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
	c := exec.Command("Clear")
	c.Stdout = os.Stdout
	c.Run()

	println(afterClearMsg)
}
