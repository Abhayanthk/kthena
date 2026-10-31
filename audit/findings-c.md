# Audit C findings: router datastore

Scope: `pkg/kthena-router/datastore/store.go`, `fairness_queue.go` and `session_boost_queue.go`, as restored unchanged from upstream main at `348681bb` (the files on `audit-c-head` are byte-identical to `348681bb`, so every `file:line` below is valid on upstream main).

The review ran nine independent finder angles (line-by-line, removed-behavior/history, cross-file tracing, Go pitfalls, wrapper consistency, reuse, simplification, efficiency, altitude), deduplicated their candidates and verified each one. 25 findings survived, plus 2 extra lower-severity findings from the final sweep (26 and 27). Issues on the "already known" list of the audit PR (PR 1921, PR 1837, PR 1461, PR 1542, PR 1772, PR 1358, PR 1709, PR 1782) were excluded.

Verdicts: **CONFIRMED** means the inputs and event order that trigger the bug and its wrong result were demonstrated. **PLAUSIBLE** means the mechanism is real but the production trigger depends on timing, configuration or policy, which is stated.

## How to run the scratch tests

Scratch tests are kept under `audit/scratch-c/` with a `.go.txt` extension so they never compile as part of the module. They live in `package datastore` and use unexported symbols, so run them in a temporary copy of the package:

```bash
cp -r pkg/kthena-router/datastore pkg/kthena-router/zzaudit && cp audit/scratch-c/<FILE>.go.txt pkg/kthena-router/zzaudit/zz_scratch_test.go && go test ./pkg/kthena-router/zzaudit/ -run '<TEST>' -count=1 -v; rm -rf pkg/kthena-router/zzaudit
```

Copy one scratch file at a time; the files are independent and may define overlapping helpers. Files:

- `zz_audit_a_test.go.txt` (tests named `TestZZA_*`)
- `zz_alt_test.go.txt` (tests named `TestAlt_*`)
- `zz_eff_test.go.txt` (tests named `TestEff_*`; several measure timing and take a few seconds)
- `zz_extra_test.go.txt` (tests named `TestExtra_*`)

All four compile against the package at `348681bb`. Some findings were reproduced by reviewer scratch tests that were deleted after the run and not retained; those entries say so and describe the repro so it can be rewritten.

Production goroutines referred to below:

- **request handlers**: the gin handler in `router/router.go` (HasModel check, `handleFairnessScheduling`, `doLoadbalance`, proxy loop).
- **controller worker**: each router controller runs a single worker (`go wait.Until(c.runWorker, ...)`); the ModelServer controller's one workqueue carries both ModelServer and Pod items.
- **ModelRoute controller worker** (or the file source's sync loop in `--config-source=file` mode).
- **scrape loop**: the goroutine started by `store.Run`.
- **queue worker**: one `RequestPriorityQueue.Run` goroutine per model name.
- **callback goroutines**: one per callback per event, started by `triggerCallbacks`.

## Summary

| # | Title | Location | Verdict |
|---|---|---|---|
| 1 | Session-boost capacity gate matches pods by the requested model name | store.go:765 | CONFIRMED |
| 2 | Redis on-flight counter leaks +1 when Decr falls back to local | store.go:916 | CONFIRMED |
| 3 | Replacing a ModelServer's pod set leaves dropped pods bound and leaked | store.go:950 | CONFIRMED |
| 4 | Heap rebuild resets anonymous priority from lowest to highest | fairness_queue.go:479 | CONFIRMED |
| 5 | PD indexes not rebuilt when the pod set is replaced | store.go:952 | CONFIRMED |
| 6 | New-pod scrape blocks the single controller worker | store.go:1193 | CONFIRMED |
| 7 | Scrape cycle barrier lets one slow pod stall every pod's refresh | store.go:644 | CONFIRMED |
| 8 | Deleting one ModelRoute drops the rate limiter other routes still use | store.go:1474 | CONFIRMED |
| 9 | Callbacks run in per-event goroutines and lose ordering | store.go:1944 | CONFIRMED |
| 10 | Enqueue recreates a queue after its route was deleted | store.go:732 | CONFIRMED |
| 11 | Route tie-break compares ResourceVersion strings | store.go:1455 | CONFIRMED |
| 12 | 100ms Redis context timeouts are not enforced | store.go:886 | CONFIRMED |
| 13 | A gauge missing from a successful scrape is stored as 0 | store.go:1911 | CONFIRMED |
| 14 | ModelRoute sectionName ignores the matched listener | store.go:1603 | CONFIRMED |
| 15 | DeleteModelRoute closes a queue still served through the LoRA index | store.go:1468 | CONFIRMED |
| 16 | ModelRoute parentRef ignores the Gateway's allowedRoutes | store.go:1601 | PLAUSIBLE |
| 17 | inferenceEngine change never reaches existing PodInfos | store.go:1220 | CONFIRMED |
| 18 | FAIRNESS_MAX_QPS above 1e9 panics NewTicker | fairness_queue.go:530 | CONFIRMED |
| 19 | Changing a route's modelName leaves the old name's limiter | store.go:1281 | PLAUSIBLE |
| 20 | Busy-backend log is unconditional INFO on every check | store.go:782 | CONFIRMED |
| 21 | Per-pod recheck fan-out is O(pods²) per scrape cycle | store.go:635 | CONFIRMED |
| 22 | Blocked pop admits the next arrival without a capacity check | session_boost_queue.go:397 | CONFIRMED |
| 23 | Every held signal scans the whole heap under pq.mu | session_boost_queue.go:373 | PLAUSIBLE |
| 24 | Semaphore mode commits admission before a permit frees | fairness_queue.go:553 | CONFIRMED |
| 25 | Re-created same-name pod is treated as an in-place update | store.go:1159 | PLAUSIBLE |
| 26 | All-zero-weight rule shadows later ModelRoutes | store.go:1544 | PLAUSIBLE |
| 27 | Nil header matcher panics in matchString | store.go:1637 | PLAUSIBLE |

---

## 1. Session-boost capacity gate matches pods by the requested model name

- **Location:** `pkg/kthena-router/datastore/store.go:765` (same defect at :799; fallback at :778-780; used at `session_boost_queue.go:361-365`)
- **Verdict:** CONFIRMED (found independently by 8 of 9 angles)
- **Failure scenario:** `makeBackendWaitingChecker` and `makeBackendPodCounter` select pods with `podInfo.Contains(modelName)`, where `modelName` is the queue key, i.e. the ModelRoute `spec.modelName` the client requested (`router.go:1698`). `PodInfo.models` holds the names the pods actually serve, from `/v1/models` (ModelServer `spec.model`); the router rewrites the request body to that name only after dequeue (`router.go:536`). With an aliased route no pod matches:
  - the checker takes `if podCount == 0 { return true }`, so backpressure never holds even when every pod reports `RequestWaitingNum > 0`;
  - the pod counter returns 0, so `maxInflight` stays `InflightPerPod` (16) for the whole model instead of 16 × pods.

  With the shipped examples (`examples/kthena-router/ModelRouteSimple.yaml`, modelName `deepseek-simple` → `ModelServer-ds1.5b.yaml`, model `deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B`) and 4 busy pods, 16 of 40 queued requests were admitted; the docs promise 0 now and a limit of 64. Excess requests wait and return 504 after `SESSION_BOOST_TIMEOUT`. The file's own comment at store.go:830-833 says queue keys "need not match the model name the backend pods serve". The checker also counts unrelated pods that serve the same name (another ModelServer or namespace, or leaked PodInfos from finding 3), so one idle unrelated pod opens the gate.
- **Reachability:** `ENABLE_SESSION_BOOST=true` with any ModelRoute whose `modelName` differs from the served model name (every example in `examples/kthena-router`). Closures are built in `Enqueue` (request handlers) and run by the queue worker on every arrival, release and recheck.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_SessionBoostModelNameMismatch`; `zz_alt_test.go.txt` `TestAlt_AliasedModelDisablesSessionBoostBackpressure`, `TestAlt_IdlePodOfOtherModelServerOpensGate`; `zz_eff_test.go.txt` `TestEff_SessionBoostAliasCap`.

## 2. Redis on-flight counter leaks +1 when Decr falls back to local

- **Location:** `pkg/kthena-router/datastore/store.go:916` (Incr mirror at :888-895; sync overwrite at :696-699)
- **Verdict:** CONFIRMED
- **Failure scenario:** `IncrPodOnFlightRequests` and `DecrPodOnFlightRequests` each choose Redis or the local atomic independently on every call.
  1. Incr succeeds in Redis (pod P = 1).
  2. Redis has a blip longer than 100ms (failover, pool wait, network error) while the request streams.
  3. Decr errors, so only the local counter is decremented. Redis keeps P = 1 permanently; only `DeletePod`'s HDEL or the negative clamp ever changes it.
  4. The next `SyncOnFlightCounts` (at most 50ms later, `scheduler_impl.go:124`) copies 1 into the local counter on every router replica.

  `least_request.go:119` scores `onFlight + 100*max(onFlight-running, 0)`, so an idle pod scores about 101 against 0 and is starved by every router. Each failed Decr adds another +1. The reverse case (Incr falls back to local, Decr reaches Redis) subtracts another replica's in-flight request.
- **Reachability:** `REDIS_HOST` set, least-request scoring (default plugin set). Request handlers (proxy loop `router.go:986/1000`, PD hooks `router.go:1562-1565`) and the scheduler.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_RedisDecrFailureLeaksGlobalCount` (miniredis with injected errors: local=1, redis=1 after sync with nothing in flight); `zz_alt_test.go.txt` `TestAlt_RedisIncrLocalDecrDrift`.

## 3. Replacing a ModelServer's pod set leaves dropped pods bound and leaked

- **Location:** `pkg/kthena-router/datastore/store.go:950` (DeleteModelServer walk at :975; eviction at :982-983)
- **Verdict:** CONFIRMED
- **Failure scenario:** on update, `AddOrUpdateModelServer` replaces `ms.pods` but never removes the ModelServer from the dropped pods' `PodInfo.modelServer`. The comment at :946-949 defers this to "the pod handler", which never runs when only the ModelServer changed, and does nothing for a pod that matches no ModelServer (`modelserver_controller.go:305`). `DeleteModelServer` only walks the current `ms.pods`.
  - Blue/green: the selector changes `app=v1` → `app=v2` while v1 pods stay Ready. The v1 PodInfos keep `modelServers=[ms]`. After `DeleteModelServer(ms)` they stay in `s.pods` until each pod is deleted or goes NotReady. Meanwhile they are scraped every interval, included in `SyncOnFlightCounts`, returned by `GetPodsByInferencePool` and the debug dump, and, being idle (waiting 0), make the session-boost checker report capacity and inflate the pod counter.
  - Permanent variant: a ModelServer switched from `spec.endpoints` to `workloadSelector` leaves its synthetic `<ms>:<endpoint>` PodInfos forever, because no Kubernetes pod event ever deletes them.
- **Reachability:** ModelServer/Pod controller worker (cause); scrape loop and session-boost queue worker (effects). No informer resync (`cmd/kthena-router/app/controller.go:80-82` uses 0) repairs it.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_SelectorChangeLeaksPodInfo`; `zz_alt_test.go.txt` `TestAlt_SelectorChangeLeavesZombie`, `TestAlt_EndpointsToSelectorLeavesZombiePodInfo`.

## 4. Heap rebuild resets anonymous priority from lowest to highest

- **Location:** `pkg/kthena-router/datastore/fairness_queue.go:479` (also the dequeue-time refresh at :398-399; rebuild trigger at :406-409)
- **Verdict:** CONFIRMED
- **Failure scenario:** the router gives requests without a user ID `math.MaxFloat64` (`router.go:1693`, "lowest priority to unauthenticated requests so they don't starve authenticated users"). `rebuildHeap` and the dequeue-time refresh recompute every queued priority with `CalculateFairnessPriority`, which returns 0 for user `""` (`token_tracker.go:219-220`). Queue: alice at stale 100 (now 200 tokens), bob at 150, 3 anonymous. Popping alice exceeds bob, retries hit the max, `rebuildHeap` runs (depth ≤ 64), and the anonymous priorities become 0. Dequeue order is anon, anon, anon, bob, alice; with refresh disabled it is alice, bob, anon×3. This is distinct from PR 1772: the cause is the sentinel being overwritten, not same-user FIFO.
- **Reachability:** `ENABLE_FAIRNESS_SCHEDULING=true`, `FAIRNESS_PRIORITY_REFRESH_RETRIES >= 1` (the user guide suggests 1 or 2), queue depth ≤ `FAIRNESS_REBUILD_THRESHOLD`, and mixed authenticated and anonymous traffic. `AuthMiddleware` authenticates only `/v1/` paths while the gateway handler serves `/*path`, so unauthenticated requests can share a per-model queue. Queue worker.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_AnonymousSentinelOverwrittenByRebuild`.

## 5. PD indexes not rebuilt when the pod set is replaced

- **Location:** `pkg/kthena-router/datastore/store.go:952` (pod set replaced at :950)
- **Verdict:** CONFIRMED
- **Failure scenario:** `AddOrUpdateModelServer` clears and rebuilds `pdGroups`, `decodePodGroups` and `prefillPodGroups` only when `WorkloadSelector` changed. An operator quarantines decode pod d1 by relabeling it out of the selector (it stays Ready); `syncPodHandler` finds no matching ModelServer and makes no store call. A later ModelServer event with an unchanged selector (e.g. a trafficPolicy edit) calls `AddOrUpdateModelServer(ms, {d2, p1})`. `GetPodsByModelServer` now excludes d1, but `GetDecodePods` and `GetPrefillPodsForDecodeGroup` still return it, so PD traffic keeps going to the quarantined pod until it goes NotReady or is deleted.
- **Reachability:** PD-disaggregated ModelServers. ModelServer/Pod controller worker writes the stale index; request handlers read it via `scheduler_impl.go:132/194`.
- **Repro:** `zz_alt_test.go.txt` `TestAlt_PDIndexKeepsPodDroppedFromPodSet` (ms.pods=[d2 p1], decode index [d1 d2]).

## 6. New-pod scrape blocks the single controller worker

- **Location:** `pkg/kthena-router/datastore/store.go:1193` (and :1194)
- **Verdict:** CONFIRMED
- **Failure scenario:** the new-pod path of `AddOrUpdatePod` makes two blocking HTTP GETs to the pod (`/metrics`, then `/v1/models`; the shared client has a 5s timeout, `backend/metrics/metrics.go:32-33`). It runs on the single ModelServer/Pod controller worker.
  - Steady state: one Ready-but-unresponsive pod (partitioned node, NetworkPolicy dropping router traffic) holds every Pod/ModelServer event for about 10s. Deletes of dead pods queue behind it, so those pods stay in `s.pods` and keep receiving traffic. N such pods block N × 10s.
  - Startup: `server.go:136-142` starts the HTTP listeners (including `/healthz`) only after `WaitForCacheSync`, which waits for the worker to process every initial pod. A couple of unresponsive pods, or a few hundred healthy ones, can exceed the router liveness window (`initialDelaySeconds: 1`, `periodSeconds: 5`, default failureThreshold 3) and put the router in CrashLoopBackOff. File-source mode is affected the same way through `SyncStaticEndpoints`.

  The work is also redundant: the scrape loop's first cycle re-scrapes every pod in parallel right after sync.
- **Reachability:** any config. Controller worker (`modelserver_controller.go:252/306`, `endpoints.go:60`).
- **Repro:** `zz_eff_test.go.txt` `TestEff_AddOrUpdatePodBlocksControllerWorker` (AddOrUpdatePod took 10.0s against a port that accepts and never replies; a queued DeletePod ran 10s late).

## 7. Scrape cycle barrier lets one slow pod stall every pod's refresh

- **Location:** `pkg/kthena-router/datastore/store.go:644` (per-pod goroutine at :631-640)
- **Verdict:** CONFIRMED
- **Failure scenario:** each cycle waits (`wg.Wait()`) for every pod's metrics GET followed by its models GET before taking the next tick. One slow or unresponsive Ready pod makes each cycle take about 5s + 5s, so waiting/running/KV-cache/model data for every healthy pod is about 10× staler than `METRICS_SCRAPE_INTERVAL`, and scrape-driven session-boost rechecks fire about 10× less often (more 504s). More than 100 hung pods also exhaust the 100-slot semaphore, so healthy pods wait behind them. The first `HasSynced` (`/readyz`) is delayed too. Commit 55255bd parallelized the loop to fix this starvation but kept the barrier.
- **Reachability:** any config; triggered by a node failure or partition while pods stay Ready, or a hung engine. Scrape loop.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_SlowPodThrottlesAllScrapes` (100ms interval, one 2s pod: the fast pod was scraped 3 times in 4.1s instead of about 41); `zz_eff_test.go.txt` `TestEff_ScrapeBarrier`.

## 8. Deleting one ModelRoute drops the rate limiter other routes still use

- **Location:** `pkg/kthena-router/datastore/store.go:1474` (event at :1472-1476)
- **Verdict:** CONFIRMED
- **Failure scenario:** `DeleteModelRoute` always emits `EventDelete{ModelName}`, even when other ModelRoutes still serve that model. The router keeps one limiter per model name (`router.go:162-164` calls `DeleteLimiter(data.ModelName)`). With team-a/r1 (with `rateLimit`) and team-b/r2 both on modelName `llama` (a supported topology, see e2e `TestDuplicateModelName`), deleting r2 removes r1's limiter; r1 still routes and `HasModel` stays true, but its limit is no longer enforced until r1 is edited or the router restarts (resync 0). A blue/green replacement (create r2, delete r1) gives the same state. In file-source mode, renaming a route's `metadata.name` triggers it deterministically (Add for the new key, then Delete for the old key). This is the opposite failure of PR 1709: a limiter that is still needed is deleted.
- **Reachability:** ModelRoute controller worker or file-source sync, then the callback goroutine; the missing limit is observed by request handlers at `router.go:360`.
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: add two routes with the same modelName where one has `RateLimit`, register the router's callback body against a real `ratelimit.TokenRateLimiter`, delete the route without a limit, then assert that `HasModel` is true and the limiter is gone.

## 9. Callbacks run in per-event goroutines and lose ordering

- **Location:** `pkg/kthena-router/datastore/store.go:1944` (`for _, callback := range callbacks { go callback(data) }`)
- **Verdict:** CONFIRMED
- **Failure scenario:** each callback of each event runs in a new goroutine, so back-to-back events for the same object are applied in arbitrary order, usually reversed (the Go scheduler runs the newest goroutine first).
  - ModelRoute: delete r1 then add r2 for the same model with a `rateLimit` → `DeleteLimiter` runs after `AddOrUpdateLimiter`, so the live route has no limiter. Two quick updates (limit 10 then 100) can end on 10.
  - Gateway (`ENABLE_GATEWAY_API`): delete and re-create with the same name → if Add runs first, the later Delete stops every listener of a live Gateway (`cmd/kthena-router/app/router.go:74-83`).

  Nothing resyncs either state (resync 0). Goroutine count is also unbounded under event storms.
- **Reachability:** any controller worker emitting two events for one key back-to-back (relist, requeued dirty key, or the file source's tight apply loop at `filesource/source.go:334-345`). Worst on a 1-CPU router.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_CallbackReorder`; `zz_alt_test.go.txt` `TestAlt_CallbackReorder`; `zz_eff_test.go.txt` `TestEff_CallbackReordering`, `TestEff_CallbackReorderingWithWork` (delete observed last in about 196-200 of 200 trials at GOMAXPROCS 1-8).

## 10. Enqueue recreates a queue after its route was deleted

- **Location:** `pkg/kthena-router/datastore/store.go:732` (LoadOrStore; `go newQueue.Run` at :739; cleanup after unlock at :1465-1468)
- **Verdict:** CONFIRMED
- **Failure scenario:** `Enqueue` creates a queue and starts its goroutine for any model name without re-checking the route under `routeMutex`. The handler's `HasModel` check (`router.go:314`) is a separate, earlier critical section, followed by tokenization, rate limiting and priority calculation. If the ModelRoute controller runs `DeleteModelRoute` (or an update that drops the name) in that window, `cleanRequestWaitingQueues` removes the queue, then the handler's `Enqueue` misses, calls `LoadOrStore` and starts `go newQueue.Run`. The new queue, its goroutine and (in session-boost mode) its LRU live until a route with that name is created and deleted again; `HasModel` now rejects every new request for it. This re-opens the leak that commit 681dffc fixed for issue 1613, and contradicts the comment at `router.go:404-416`.
- **Reachability:** `ENABLE_FAIRNESS_SCHEDULING` or `ENABLE_SESSION_BOOST`; request handler racing the ModelRoute controller worker. Bounded at one queue per deleted model or LoRA name per race.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_EnqueueAfterRouteDeleteLeaksQueue`; `zz_alt_test.go.txt` `TestAlt_EnqueueAfterRouteDeleteLeaksQueue` (queue present, `HasModel=false`, goroutines +1).

## 11. Route tie-break compares ResourceVersion strings

- **Location:** `pkg/kthena-router/datastore/store.go:1455` (comparator at :1453-1456; "kept sorted oldest-first" claim at :1518)
- **Verdict:** CONFIRMED
- **Failure scenario:** `sortModelRoutesInPlace` breaks equal `CreationTimestamp` values (1-second resolution) by string-comparing `ResourceVersion`, which is a last-write version, not an age.
  - RVs "9" and "10": `"10" < "9"`, so the newer route wins from the start.
  - RVs 100 (canary, header match) and 101 (catch-all): the canary wins until any write to it (a label edit, or model-booster's update at `pkg/model-booster-controller/controller/model_route_handler.go:101`) bumps it to 102. After the re-sort the catch-all comes first, and all canary traffic silently moves to the default backend.

  The HTTPRoute comparator (`router/httproute_match.go:268-286`) uses creationTimestamp, then namespace/name, which is stable.
- **Reachability:** two or more matching ModelRoutes for one model created in the same second (one `kubectl apply -f dir/` or Helm install). ModelRoute controller worker re-sorts; request handlers read the order in `matchModelRoute`.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_RouteOrderResourceVersion`; `zz_eff_test.go.txt` `TestEff_RouteOrderByResourceVersion`.

## 12. 100ms Redis context timeouts are not enforced

- **Location:** `pkg/kthena-router/datastore/store.go:886` (same pattern at :907, :689, :1261)
- **Verdict:** CONFIRMED
- **Failure scenario:** the store wraps every Redis call in `context.WithTimeout(..., 100*time.Millisecond)`, but `utils.TryGetRedisClient` (`pkg/kthena-router/utils/redis.go:32-36`) builds the client without `ContextTimeoutEnabled`. go-redis v9 then ignores the context deadline for socket reads and writes (`redis.go:570`: `if c.opt.ContextTimeoutEnabled { return ctx }; return context.Background()`) and uses the 3s default ReadTimeout. When Redis stops answering on open connections (slow Lua or command, fork/AOF stall, failover, partition after connect):
  - each Incr and Decr in the proxy loop adds up to 3s, and in fairness/session-boost mode the deferred Release holds the queue permit for that long;
  - the request that wins the `SyncOnFlightCounts` CAS blocks 3s;
  - `DeletePod` blocks the controller worker up to 3s per pod, so the last delete in a 50-pod scale-down is about 150s late and that terminated pod keeps receiving traffic.

  This also means the "100ms" bound assumed by finding 2's fallback is really 3s.
- **Reachability:** `REDIS_HOST` set. Request handlers, scheduler, controller worker.
- **Repro:** `zz_eff_test.go.txt` `TestEff_RedisCtxTimeoutIgnored` (against a TCP server that accepts and never replies: Incr 3.004s, Decr 3.003s, SyncOnFlightCounts 3.003s, DeletePod 3.001s).

## 13. A gauge missing from a successful scrape is stored as 0

- **Location:** `pkg/kthena-router/datastore/store.go:1911` (`updateFunc(podinfo, metricsInfo[name])`; success signal at :1843)
- **Verdict:** CONFIRMED
- **Failure scenario:** `updateGaugeMetricsInfo` writes `metricsInfo[name]` for every expected gauge, but the engine providers omit absent metric families (`backend/vllm/metrics.go`: `if !exist { continue }`). When `/metrics` returns 200 without the waiting/running/KV-cache gauges, `GPUCacheUsage`, `RequestWaitingNum` and `RequestRunningNum` are zeroed while `updatePodMetrics` reports success and fires a session-boost recheck. Examples: an engine exporting the KV gauge under another name, stats disabled, or an engine mismatch (finding 17). The pod then looks idle everywhere: the session-boost checker admits, the least-request waiting filter never fires, `least_request.go:119` treats every in-flight request as queued, and GPU scoring scores every pod 100. Only TPOT/TTFT have a skip-zero special case (store.go:74-85). A never-scraped pod's zero-valued `RequestWaitingNum` is treated as idle by the checker in the same way.
- **Reachability:** any config where the expected gauge names are not exported. Scrape loop and the controller worker's initial fetch.
- **Repro:** `zz_alt_test.go.txt` `TestAlt_MissingGaugeBecomesZero` (httptest with the real vLLM provider: waiting 10→0, KV 0.95→0, success=true).

## 14. ModelRoute sectionName ignores the matched listener

- **Location:** `pkg/kthena-router/datastore/store.go:1603` (listener loop at :1603-1609)
- **Verdict:** CONFIRMED
- **Failure scenario:** `matchesSpecificGateway` only checks that a parentRef's `sectionName` names some listener on the Gateway, and ignores `parentRef.port`. `MatchModelTarget` never receives the matched listener: `cmd/kthena-router/app/router.go:275-276` stores it in the context, but `router.go:459` passes only `gatewayKey`. On a Gateway with listeners `http` and `https` (or `tenant-a` and `tenant-b` by hostname), a ModelRoute bound to `sectionName: https` is served on plain `http`, and tenant-b traffic is routed by the older tenant-a route, so route-b is unreachable. HTTPRoutes already use the listener-aware `httpRouteMatchesGatewayListener` (`router/httproute_match.go:98-147`).
- **Reachability:** `ENABLE_GATEWAY_API` with multi-listener Gateways and ModelRoutes that use `sectionName`. Request handlers.
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: add a Gateway with two listeners and two ModelRoutes for one model bound to different `sectionName` values, call `MatchModelTarget(model, req, "ns/gw")`, and observe that the older route wins regardless of listener.

## 15. DeleteModelRoute closes a queue still served through the LoRA index

- **Location:** `pkg/kthena-router/datastore/store.go:1468` (unfiltered call); compare the filter at :1345-1350
- **Verdict:** CONFIRMED
- **Failure scenario:** `AddOrUpdateModelRoute` only closes queues for names with `len(s.routes[name]) == 0 && len(s.loraRoutes[name]) == 0` (filter added in commit 30b9ee4), but `DeleteModelRoute` passes the raw candidates from `removeModelRouteFromIndexesLocked` to `cleanRequestWaitingQueues`. With route A `{modelName: x}` and route B `{loraAdapters: [x]}`, deleting A closes queue x and `Cancel()`s every waiting request (503 "Request queue closed while waiting", `router.go:1754`), although `HasModel(x)` and `MatchModelTarget(x)` still succeed through B. The replacement queue starts with an empty semaphore and inflight count while A-era admissions still run, so `FAIRNESS_MAX_CONCURRENT` or the session-boost limit can be exceeded up to 2× until they drain, and session-boost history is lost.
- **Reachability:** fairness or session boost enabled, and a base model name equal to another route's LoRA adapter name (the maps are global across namespaces; the webhook allows it). ModelRoute controller worker.
- **Repro:** `zz_audit_a_test.go.txt` `TestZZA_DeleteRouteClosesLiveLoraQueue`; `zz_alt_test.go.txt` `TestAlt_DeleteModelRouteClosesStillRoutedQueue`.

## 16. ModelRoute parentRef ignores the Gateway's allowedRoutes

- **Location:** `pkg/kthena-router/datastore/store.go:1601` (namespace from parentRef at :1591-1601)
- **Verdict:** PLAUSIBLE
- **Failure scenario:** `matchesSpecificGateway` accepts any parentRef whose namespace/name equals the gateway key, without the listener `allowedRoutes.namespaces` check that the HTTPRoute controller enforces (`controller/httproute_controller.go:320-321`; Gateway API default `From: Same`). A user who can create ModelRoutes in team-b creates `{modelName: m, parentRefs: [{name: gw, namespace: team-a}]}` targeting a team-b ModelServer or ExternalModelProvider (`modelTargetFromDestination` resolves targets in `mr.Namespace`). Requests on team-a/gw for m go to team-b's backend, including team-a's prompts when team-a also routes m and team-b's route is older. The built-in default Gateway explicitly sets `From: All` (`cmd/kthena-router/app/controller.go:290-306`), and parentRef-less ModelRoutes are already limited to their own namespace (store.go:1531), which shows the attachment policy is intended. No webhook validates ModelRoute parentRefs.
- **Reachability:** `ENABLE_GATEWAY_API` in a cluster where namespaces are not mutually trusted. Request handlers. It would be confirmed by a multi-tenant RBAC setup where team-b can create ModelRoutes but not modify team-a's Gateway.
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: add Gateway team-a/gw with default allowedRoutes and a team-b ModelRoute with a cross-namespace parentRef, then call `MatchModelTarget(m, req, "team-a/gw")` and observe the team-b target.

## 17. inferenceEngine change never reaches existing PodInfos

- **Location:** `pkg/kthena-router/datastore/store.go:1220` (engine set only if empty at :1223-1224)
- **Verdict:** CONFIRMED
- **Failure scenario:** for pods that already have a PodInfo, ModelServer sync uses only `AppendModelServerToPod` (`modelserver_controller.go:245-246`), which does nothing when the pod already lists the ModelServer. So correcting `spec.inferenceEngine` from vLLM to SGLang on a live ModelServer leaves existing PodInfos on "vLLM" until each pod happens to get a Pod event (no informer resync). The scrape loop keeps parsing with the vLLM provider, gets an empty map, and through finding 13 writes 0 for KV/waiting/running every scrape. Pods look idle to least-request and KV-cache scoring, the session-boost checker always reports capacity, and the tokenizer manager uses the wrong engine (`tokenizer_manager.go:139`).
- **Reachability:** any edit of a mutable `spec.inferenceEngine` on a live ModelServer. Controller worker (cause); scrape loop and scorers (effects).
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: add a pod via `AddOrUpdatePod(pod, [ms with vLLM])`, then `AddOrUpdateModelServer(ms with SGLang, pods)` and `AppendModelServerToPod(pod, [ms])`, and observe that `GetPodInfo(pod).GetEngine()` is still "vLLM".

## 18. FAIRNESS_MAX_QPS above 1e9 panics NewTicker

- **Location:** `pkg/kthena-router/datastore/fairness_queue.go:530` (interval at :529; validation at store.go:495)
- **Verdict:** CONFIRMED
- **Failure scenario:** `FAIRNESS_MAX_QPS` is only validated as `n > 0`, and `runQPSMode` computes `interval := time.Second / time.Duration(qps)`. For qps above 1e9 the interval is 0 and `time.NewTicker` panics with "non-positive interval for NewTicker" in the queue goroutine, which nothing recovers. The router process exits on the first queued request, and again after every restart.
- **Reachability:** `ENABLE_FAIRNESS_SCHEDULING=true`, `FAIRNESS_MAX_CONCURRENT` unset or 0 (QPS mode is the default), and `FAIRNESS_MAX_QPS` set to a huge value, e.g. to "disable" the cap. Queue worker started by `Enqueue` (store.go:739).
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: set `FAIRNESS_MAX_QPS=2000000000`, build the config with `createFairnessQueueConfig()`, and call `NewRequestPriorityQueueWithConfig(nil, cfg, nil, nil).Run(ctx, cfg.MaxQPS)`; it panics immediately.

## 19. Changing a route's modelName leaves the old name's limiter

- **Location:** `pkg/kthena-router/datastore/store.go:1281` (old name discarded; event at :1356-1360 uses only the new name)
- **Verdict:** PLAUSIBLE
- **Failure scenario:** `AddOrUpdateModelRoute` throws away the old model name that `removeModelRouteFromIndexesLocked` returns, and emits `EventUpdate` only for the new `spec.modelName`, so subscribers never learn that the old name lost its route. Route r with modelName A and a `rateLimit` becomes B, or drops `modelName` to become LoRA-only: the limiter for A survives, and a later route for A without a limit is throttled by r's old limit (rate limiting runs before routing, `router.go:360`). LoRA-only routes also emit `ModelName ""`, so their adapters are never rate-limited. Same symptom as PR 1709, but a different trigger and root cause (no event for the old key).
- **Reachability:** `spec.modelName` is CEL-immutable in Kubernetes mode, so this is reachable in file-source mode (no old object, so the rule never runs), when `modelName` is removed (a property-level rule is not evaluated for an absent property), or when a delete and recreate coalesce in the ModelRoute workqueue. ModelRoute controller worker or file-source sync.
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: register the router's ModelRoute callback against a real `TokenRateLimiter`, add route r (modelName A, RateLimit), update r to modelName B, and observe the limiter for A still present.

## 20. Busy-backend log is unconditional INFO on every check

- **Location:** `pkg/kthena-router/datastore/store.go:782`
- **Verdict:** CONFIRMED
- **Failure scenario:** `makeBackendWaitingChecker` logs `klog.Infof("[BackendWaitingChecker] model %s: all %d pods busy, ...")` (not V-gated) every time all pods are busy, and the checker runs on every arrival, release and scrape recheck. Under sustained backpressure that is about one line per request, plus around 1000 lines/s from scrape rechecks at 1000 pods. At 1000 req/s that is roughly 230 KB/s, about 20 GB/day per replica, and with kubelet's default 10Mi×5 rotation only about 4 minutes of logs survive, losing the incident's own error logs. `router.go:1736-1744` deliberately logs session-boost timeouts at V(2) "to avoid flooding the logs" in exactly this situation.
- **Reachability:** `ENABLE_SESSION_BOOST=true`, all pods serving the model report waiting > 0 while this router is under its inflight limit. Queue worker.
- **Repro:** `zz_eff_test.go.txt` `TestEff_SessionBoostRecheckAndLogFlood` (2000 arrivals produced 1999 lines).

## 21. Per-pod recheck fan-out is O(pods²) per scrape cycle

- **Location:** `pkg/kthena-router/datastore/store.go:635` (also :638; fan-out at :834-843; scans at :760 and :794)
- **Verdict:** CONFIRMED
- **Failure scenario:** each successful per-pod metrics or models refresh calls `recheckSessionBoostQueues`, which signals every queue with held requests, including queues whose model none of those pods serve. Each signal re-runs the pod counter and the checker, and both scan every pod in the store under a PodInfo read lock. That is about 2P wakes per held queue per cycle at O(P) each, so O(P²·Q) per cycle (before commit 4e2fc66 it was one recheck per cycle). Measured at 1000 pods with one held request: about 1043 checker+counter runs per second, using about 27% of one core. With the chart's default 500m CPU limit, a couple of held queues push the router into CPU throttling, which slows proxying for every model during overload. It also hides call sites that forget to recheck, because the broadcast covers them within one interval.
- **Reachability:** `ENABLE_SESSION_BOOST=true` with held requests and many pods. Scrape loop and queue workers.
- **Repro:** `zz_alt_test.go.txt` `TestAlt_RecheckAmplification`; `zz_eff_test.go.txt` `TestEff_RecheckCostLargeFleet`, `TestEff_RecheckCostJitter`.

## 22. Blocked pop admits the next arrival without a capacity check

- **Location:** `pkg/kthena-router/datastore/session_boost_queue.go:397` (gate checks at :371-394; blocking select in `fairness_queue.go:436-445`)
- **Verdict:** CONFIRMED
- **Failure scenario:** `tryBackpressureDequeue` checks the inflight limit and `backendChecker`, then calls the blocking `popWhenAvailable`. If the heap holds only cancelled requests (held requests that timed out), `popWhenAvailable` skips them and blocks on `notifyCh`. When backends saturate meanwhile, the next arrival is popped and admitted immediately, while `backendChecker()` returns false, with a `maxInflight` computed before the block, and without the grace-period hold. One ungated admission per occurrence.
- **Reachability:** `ENABLE_SESSION_BOOST=true` under overload with request timeouts or client disconnects. Session-boost queue worker, fed by request handlers and the scrape loop's recheck.
- **Repro:** `zz_alt_test.go.txt` `TestAlt_BlockedPopAdmitsPastBusyBackend` (r2 admitted while the checker returned false; the checker call count did not change).

## 23. Every held signal scans the whole heap under pq.mu

- **Location:** `pkg/kthena-router/datastore/session_boost_queue.go:373` (also :382; scan at :320-346)
- **Verdict:** PLAUSIBLE
- **Failure scenario:** every signal that ends in "hold" (inflight limit reached or backends busy) runs `drainCancelledLocked`, an O(n) scan of every request's cancel channel plus `heap.Init`, under `pq.mu`, even when nothing was cancelled. Under sustained overload the heap grows to arrival rate × `SESSION_BOOST_TIMEOUT` (e.g. 30,000 at 1000 req/s for 30s). Measured: one scan holds `pq.mu` for 1.25ms at 30,000 entries; with about one scan per signal the worker spins near a full core and `PushRequest` from request handlers waits up to about 2ms for the lock. At roughly 500 req/s offered to one model, the scanning alone exceeds the router's default 500m CPU limit.
- **Reachability:** `ENABLE_SESSION_BOOST=true` under sustained overload. Queue worker and request handlers. The severity depends on load actually reaching that queue depth, which is why it is PLAUSIBLE.
- **Repro:** `zz_eff_test.go.txt` `TestEff_DrainScanUnderLock`.

## 24. Semaphore mode commits admission before a permit frees

- **Location:** `pkg/kthena-router/datastore/fairness_queue.go:553` (pop at :553; permit wait at :564-573)
- **Verdict:** CONFIRMED
- **Failure scenario:** `runSemaphoreMode` pops the best request and then blocks on `pq.sem` while holding it, so the admission choice is fixed up to one request duration early. With `FAIRNESS_MAX_CONCURRENT=1` and a saturated queue, a heavy user's request is popped and waits; a light user's request (much lower priority value) arrives; when the permit frees, the heavy request is admitted first. Dequeue-time priority refresh cannot re-check the held request. Metrics are also recorded at pop time: the queue-size gauge leaves out the held request, queue duration and the dequeue counter (:428-429) leave out the permit wait, and a request abandoned during the wait counts as dequeued rather than cancelled. Low severity: limited to one pre-selected request at a time.
- **Reachability:** `ENABLE_FAIRNESS_SCHEDULING=true` with `FAIRNESS_MAX_CONCURRENT > 0` and a saturated queue. Queue worker and request handlers.
- **Repro:** reproduced by a reviewer scratch test that was not retained. To rewrite: create a queue with `MaxConcurrent: 1`, admit and hold one request, push a heavy-priority request (it gets popped and blocks on the semaphore), push a light-priority request, release the first permit, and observe that the heavy request is admitted first.

## 25. Re-created same-name pod is treated as an in-place update

- **Location:** `pkg/kthena-router/datastore/store.go:1159` (in-place branch at :1159-1178)
- **Verdict:** PLAUSIBLE
- **Failure scenario:** `AddOrUpdatePod` updates an existing PodInfo by name without comparing UIDs. When a pod is deleted and re-created with the same name (StatefulSet) and the delete and add coalesce into one workqueue item, for example while the single worker is blocked by finding 6, the new pod inherits the old generation's models, metrics and on-flight count, and no Pod `EventDelete` is emitted. The prefix store then keeps the old pod's prefix entries pointing at the new, cold pod, and stale metrics and on-flight counts drive scheduling until the next scrape. Histogram resets are already handled by `LastPeriodAvg`. Distinct from PR 1837, which is about the decrement landing on the wrong PodInfo generation.
- **Reachability:** StatefulSet-style workloads where delete, re-create and Ready all happen before the worker processes the key. Controller worker. Production frequency depends on worker latency.
- **Repro:** `zz_alt_test.go.txt` `TestAlt_SameNameNewUIDIsAnUpdate`.

## 26. All-zero-weight rule shadows later ModelRoutes

- **Location:** `pkg/kthena-router/datastore/store.go:1544` (validation in `matchModelRoute`); failure raised at :1803-1804 in `selectFromWeightedSlice`
- **Verdict:** PLAUSIBLE
- **Failure scenario:** `matchModelRoute` is built to fall through to the next ModelRoute when a route cannot serve a request, and it pre-validates the matched rule with `toWeightedSlice`, but that does not reject a total weight of 0. A route whose matching rule has all targets at weight 0 is therefore selected, then `selectDestination` fails with "total weight is zero" and `MatchModelTarget` returns an error, so a valid newer route for the same model is never tried and the request fails. The CRD allows `weight: 0` (`Minimum=0`).
- **Reachability:** mostly guarded. The router's validating webhook rejects a total weight of 0 (`webhook/validator.go:252-253`), is enabled by default (`--enable-webhook=true`) with `failurePolicy: Fail`, and the file source runs the same validation (`filesource/source.go:270`). Reachable only in Kubernetes mode with the webhook disabled, or for objects admitted before the check existed. Request handlers.
- **Repro:** `zz_extra_test.go.txt` `TestExtra_ZeroWeightRouteShadowsFallthrough` (older route with `weight: 0`, newer valid route: `MatchModelTarget` returns "total weight is zero").

## 27. Nil header matcher panics in matchString

- **Location:** `pkg/kthena-router/datastore/store.go:1637` (`s.matchString(sm, reqValue)` in `selectRule`); nil dereference at :1727
- **Verdict:** PLAUSIBLE
- **Failure scenario:** `selectRule` passes each `rule.ModelMatch.Headers` value to `matchString` without a nil check, and `matchString` dereferences `sm.Exact` immediately. A ModelRoute with `headers: {x-tenant: null}` decodes to a nil `*StringMatch` and every request for that model panics. `gin.Recovery` turns it into a 500, so the router does not crash, but the model becomes unroutable through that rule, and earlier-sorted routes with this shape block later ones. `collectRouteRegexes` and the webhook both guard `sm != nil`, but the webhook only skips nil values instead of rejecting them, so validation passes.
- **Reachability:** file-source mode, where the YAML is decoded and validated by the same webhook code with no API-server pruning. In Kubernetes mode the API server likely drops null map values for this non-nullable schema; not verified. Request handlers.
- **Repro:** `zz_extra_test.go.txt` `TestExtra_NilHeaderMatcherPanics` (recovers "invalid memory address or nil pointer dereference").

---

## Checked and not reported

- **Queued requests stranded at shutdown.** The queue Run loops return on root-context cancellation without cancelling queued requests (only `Close()` does). Dropped because `cmd/kthena-router/main.go` returns as soon as `server.Run` sees `ctx.Done()` (`server.go:146-148`), so the process exits at SIGTERM and no handler is left waiting.
- **Non-transitive fairness `Less`.** Same-user FIFO mixed with cross-user priority can form a cycle, so a user's later request may be popped first. Treated as part of the known PR 1772.
- **`s.rootCtx` race.** Written in `Run` and read in `Enqueue`, but `server.go:140-142` runs `store.Run` before `startRouter`, so there is no concurrent access in production.
- **Lock ordering.** No recursive RLock or lock-order inversion: `routeMutex` is always taken before `gatewayMutex`, and `modelServer.mutex` before `PodInfo.mutex`. `NotifyChan` is never closed twice.
- **`shouldRebuildLocked` direction.** Rebuilding only at or below `FAIRNESS_REBUILD_THRESHOLD` matches the user guide and the proposal.
- **`gatewayModelRoutes`.** Its Kind check diverges from `isGatewayParentRef`, but the map has no production reader (only `store_test.go` reads it).
- **`GetRequestWaitingQueueStats`.** Reads `queue.Len()` without `pq.mu`, but has no production caller.
