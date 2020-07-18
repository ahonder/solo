package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"math/big"
	"net/http"
	"strconv"
	"sync"

	"github.com/flexpool/solo/log"
	"github.com/flexpool/solo/nodeapi"
	"github.com/flexpool/solo/utils"

	"github.com/sirupsen/logrus"
)

// OrderedWorkMap is used to store work history, and have an ability to prune unneeded work
type OrderedWorkMap struct {
	Map   map[string][]string
	Order []string
	Mux   sync.Mutex
}

// Init initializes the OrderedWorkMap
func (o *OrderedWorkMap) Init() {
	o.Map = make(map[string][]string)
}

// Append appends new work to the OrderedWorkMap
func (o *OrderedWorkMap) Append(headerHash string, work []string) {
	o.Mux.Lock()
	o.Map[headerHash] = work
	o.Order = append(o.Order, headerHash)
	o.Mux.Unlock()
}

// Shift removes the first OrderedWorkMap key
func (o *OrderedWorkMap) Shift() {
	o.Mux.Lock()
	headerHash := o.Order[0]
	delete(o.Map, headerHash)
	o.Order = o.Order[1:]
	o.Mux.Unlock()
}

// Len returns the OrderedWorkMap length
func (o *OrderedWorkMap) Len() int {
	o.Mux.Lock()
	out := len(o.Order)
	o.Mux.Unlock()
	return out
}

// WorkManager is a struct for the work manager daemon
type WorkManager struct {
	httpServer        *http.Server
	shuttingDown      bool
	subscriptions     []chan []string
	subscriptionsMux  sync.Mutex
	lastWork          []string
	workHistory       OrderedWorkMap
	shareDiff         uint64
	shareTargetHex    string
	shareTargetBigInt *big.Int
	shareDiffBigInt   *big.Int
	BestShareTarget   *big.Int
	Node              *nodeapi.Node
}

// GetLastWork returns last work
func (w *WorkManager) GetLastWork(applyShareDiff bool) []string {
	work := w.lastWork
	// Apply Share Diff
	if applyShareDiff {
		work[2] = w.shareTargetHex
	}

	return work
}

// NewWorkManager creates new WorkReceiver instance
func NewWorkManager(bind string, shareDiff uint64, node *nodeapi.Node) *WorkManager {
	shareTargetBigInt := big.NewInt(0).Div(big.NewInt(0).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0)), big.NewInt(0).SetUint64(shareDiff))
	workManager := WorkManager{
		shareDiff:         shareDiff,
		shareDiffBigInt:   big.NewInt(0).SetUint64(shareDiff),
		shareTargetBigInt: shareTargetBigInt,
		shareTargetHex:    "0x" + hex.EncodeToString(utils.PadByteArrayStart(shareTargetBigInt.Bytes(), 32)),
		lastWork:          []string{"0x0", "0x0", "0x0", "0x0"},
		BestShareTarget:   big.NewInt(0).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0)),
		Node:              node,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			log.Logger.WithFields(logrus.Fields{
				"prefix":   "workreceiver",
				"expected": "POST",
				"got":      r.Method,
			}).Error("Invalid HTTP method")
			return
		}
		data, err := ioutil.ReadAll(r.Body)
		var parsedJSONData []string
		err = json.Unmarshal(data, &parsedJSONData)
		if err != nil {
			log.Logger.WithFields(logrus.Fields{
				"prefix": "workreceiver",
				"error":  err,
			}).Error("Unable to parse OpenEthereum work notification")
			return
		}

		if len(parsedJSONData) != 4 {
			log.Logger.WithFields(logrus.Fields{
				"prefix":   "workreceiver",
				"expected": 4,
				"got":      len(parsedJSONData),
			}).Error("Invalid work notification length (Ensure that you're using OpenEthereum)")
			return
		}

		var channelIndexesToClean []int

		workManager.lastWork = parsedJSONData

		workWithShareDifficulty := parsedJSONData
		workWithShareDifficulty[2] = workManager.shareTargetHex

		// Sending work notification to all subscribers
		workManager.subscriptionsMux.Lock()
		for i, ch := range workManager.subscriptions {
			if !isChanClosed(ch) {
				ch <- parsedJSONData
			} else {
				channelIndexesToClean = append(channelIndexesToClean, i)
			}
		}

		length := len(workManager.subscriptions)

		for _, chIndex := range channelIndexesToClean {
			workManager.subscriptions[chIndex] = workManager.subscriptions[length-1]
			workManager.subscriptions = workManager.subscriptions[:length-1]
		}
		workManager.subscriptionsMux.Unlock()

		workManager.workHistory.Append(parsedJSONData[0], parsedJSONData)

		if workManager.workHistory.Len() > 8 {
			// Removing unneeded (9th in history) work
			workManager.workHistory.Shift()
		}

		log.Logger.WithFields(logrus.Fields{
			"prefix":      "workreceiver",
			"header-hash": parsedJSONData[0][2:10],
		}).Info("New job for #" + strconv.FormatUint(utils.MustSoftHexToUint64(parsedJSONData[3]), 10))
	})

	workManager.httpServer = &http.Server{
		Addr:    bind,
		Handler: mux,
	}

	workManager.workHistory.Init()

	return &workManager
}

// Run function runs the WorkReceiver
func (w *WorkManager) Run() {
	err := w.httpServer.ListenAndServe()

	if !w.shuttingDown {
		panic(err)
	}
}

// Stop function stops the WorkReceiver
func (w *WorkManager) Stop() {
	err := w.httpServer.Shutdown(context.Background())
	if err != nil {
		panic(err)
	}
}

// SubscribeNotifications subscribes the given channel to the work receiver
func (w *WorkManager) SubscribeNotifications(ch chan []string) {
	w.subscriptions = append(w.subscriptions, ch)
}

func isChanClosed(ch <-chan []string) bool {
	select {
	case <-ch:
		return true
	default:
	}

	return false
}