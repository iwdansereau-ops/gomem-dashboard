// Command sample-processor is a reference Go service used to exercise
// gomem-dashboard end-to-end. It exposes /debug/pprof on :6060 and
// intentionally leaks memory in a couple of well-known functions so the
// diff report has interesting data to display.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"sync"
	"time"
)

// registerMemStats exposes runtime.MemStats as JSON at /debug/memstats.
// This is what `gomem capture` polls to derive TotalAlloc/NumGC deltas;
// production services can drop this same two-line snippet into their
// startup code (or reuse an existing expvar handler).
func registerMemStats() {
	http.HandleFunc("/debug/memstats", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&ms)
	})
}

// leakyCache never evicts entries — a classic real-world Go leak.
var leakyCache = struct {
	sync.Mutex
	data map[string][]byte
}{data: make(map[string][]byte)}

// eventQueue grows unbounded — simulates a slow consumer.
var eventQueue [][]byte

// processBatch pretends to process incoming records and stashes results
// in leakyCache. This function is where the "flat" delta will land.
func processBatch(id string, size int) {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i)
	}
	leakyCache.Lock()
	leakyCache.data[id+"-"+time.Now().Format(time.RFC3339Nano)] = buf
	leakyCache.Unlock()
}

// enqueueEvents simulates a producer.
func enqueueEvents(n, size int) {
	for i := 0; i < n; i++ {
		eventQueue = append(eventQueue, make([]byte, size))
	}
}

// runProcessor is the top-level loop; the call graph will show
// runProcessor → processBatch and runProcessor → enqueueEvents.
func runProcessor() {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	i := 0
	for range tick.C {
		processBatch("job", 64*1024)   // 64 KB per tick → primary leak
		enqueueEvents(4, 8*1024)       // 4 × 8 KB per tick → secondary leak
		i++
		if i%50 == 0 {
			log.Printf("processed %d batches, cache=%d entries, queue=%d",
				i, len(leakyCache.data), len(eventQueue))
		}
	}
}

// churnLoop simulates the *opposite* failure mode: heavy short-lived
// allocations that GC reclaims each cycle. inuse_space stays roughly flat
// but TotalAlloc and NumGC climb aggressively. Enabled with MODE=churn.
func churnLoop() {
	var sink [][]byte
	for {
		sink = make([][]byte, 0, 1024)
		for i := 0; i < 1024; i++ {
			sink = append(sink, make([]byte, 8*1024)) // 8 MB total per pass
		}
		_ = sink // used, then dropped on next iteration
		time.Sleep(10 * time.Millisecond)
	}
}

func main() {
	registerMemStats()
	switch os.Getenv("MODE") {
	case "churn":
		log.Println("MODE=churn: high-alloc/low-retain workload")
		go churnLoop()
	default:
		log.Println("MODE=leak (default): retention workload")
		go runProcessor()
	}
	log.Println("sample-processor listening on :6060 (pprof at /debug/pprof, memstats at /debug/memstats)")
	log.Fatal(http.ListenAndServe(":6060", nil))
}
