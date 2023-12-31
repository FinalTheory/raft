package kvraft

import (
	"6.5840/labgob"
	"6.5840/labrpc"
	"6.5840/raft"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const Debug = false

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}

const (
	RPCTimeout = 60
)

type Op struct {
	Key        string
	Value      string
	Op         string
	ToClientCh chan string
	From       int
	ClientId   int64
	SeqNumber  int32
}

func (op *Op) RequestId() RequestId {
	return RequestId{ClientId: op.ClientId, SeqNumber: op.SeqNumber}
}

type RequestId struct {
	ClientId  int64
	SeqNumber int32
}

type RequestInfo struct {
	requestId RequestId
	failCh    chan bool
}

type KVServer struct {
	mu      sync.Mutex
	rwLock  sync.RWMutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	state           map[string]string
	dedupTable      map[int64]int32
	valueTable      map[int64]string
	pendingRequests map[int]RequestInfo
	killCh          chan bool
	termCh          chan int
}

// ShouldStartCommand returns whether to accept this RPC
func (kv *KVServer) ShouldStartCommand(clientId int64, newSeq int32, err *Err, value *string) bool {
	kv.rwLock.RLock()
	defer kv.rwLock.RUnlock()
	seq, ok := kv.dedupTable[clientId]
	if ok {
		switch {
		case newSeq == seq:
			*err = OK
			if value != nil {
				*value = kv.valueTable[clientId]
			}
			return false
		case newSeq < seq:
			*err = ErrStaleRequest
			return false
		case newSeq > seq:
			return true
		}
	}
	// we should start command if client ID not in dedup table
	return true
}

func (kv *KVServer) RecordRequestAtIndex(index int, id RequestId, failCh chan bool) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	// there could be multiple requests from different clients accepted by current leader, and indices recorded into `pendingRequests`
	// and then it's no longer a leader, thus these entries are finally overridden by another new leader
	// at commit stage, current server will detect different operations are committed at these recorded indices
	// thus wake up corresponding RPC request and fail them to force client retry.
	kv.pendingRequests[index] = RequestInfo{requestId: id, failCh: failCh}
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	if !kv.ShouldStartCommand(args.ClientId, args.SeqNumber, &reply.Err, &reply.Value) {
		return
	}
	clientCh := make(chan string, 1)
	failCh := make(chan bool, 1)
	index, _, isLeader := kv.rf.Start(Op{
		Key:        args.Key,
		Op:         GetOp,
		ToClientCh: clientCh,
		From:       kv.me,
		ClientId:   args.ClientId,
		SeqNumber:  args.SeqNumber,
	})
	if isLeader {
		DPrintf("[%d] Get start index=%d ClientId:%d SeqNumber:%d", kv.me, index, args.ClientId, args.SeqNumber)
		defer DPrintf("[%d] Get end index=%d ClientId:%d SeqNumber:%d", kv.me, index, args.ClientId, args.SeqNumber)
		kv.RecordRequestAtIndex(index, RequestId{ClientId: args.ClientId, SeqNumber: args.SeqNumber}, failCh)
		// block until it's committed
		select {
		case <-failCh:
			reply.Err = ErrLostLeadership
		case result := <-clientCh:
			reply.Value = result
			reply.Err = OK
		case <-time.After(time.Second * RPCTimeout):
			reply.Err = ErrTimeOut
		}
	} else {
		reply.Err = ErrWrongLeader
	}
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	if !kv.ShouldStartCommand(args.ClientId, args.SeqNumber, &reply.Err, nil) {
		return
	}
	clientCh := make(chan string, 1)
	failCh := make(chan bool, 1)
	index, _, isLeader := kv.rf.Start(Op{
		Key:        args.Key,
		Value:      args.Value,
		Op:         args.Op,
		ToClientCh: clientCh,
		From:       kv.me,
		ClientId:   args.ClientId,
		SeqNumber:  args.SeqNumber,
	})
	if isLeader {
		DPrintf("[%d] PutAppend start index=%d ClientId:%d SeqNumber:%d", kv.me, index, args.ClientId, args.SeqNumber)
		defer DPrintf("[%d] PutAppend end index=%d ClientId:%d SeqNumber:%d", kv.me, index, args.ClientId, args.SeqNumber)
		kv.RecordRequestAtIndex(index, RequestId{ClientId: args.ClientId, SeqNumber: args.SeqNumber}, failCh)
		// block until it's committed
		select {
		case <-failCh:
			reply.Err = ErrLostLeadership
		case <-clientCh:
			reply.Err = OK
		case <-time.After(time.Second * RPCTimeout):
			reply.Err = ErrTimeOut
		}
	} else {
		reply.Err = ErrWrongLeader
	}
}

func (kv *KVServer) OperationExecutor() {
	for !kv.killed() {
		DPrintf("[%d] waiting for op", kv.me)
		select {
		// receives committed raft log entry
		case cmd := <-kv.applyCh:
			if cmd.TermChanged {
				kv.FailAllPendingRequests()
				continue
			}
			if !cmd.CommandValid {
				continue
			}
			kv.FailConflictPendingRequests(cmd)
			op := cmd.Command.(Op)
			DPrintf("[%d] Apply command [%d] [%+v]", kv.me, cmd.CommandIndex, op)
			result := kv.ApplyOperation(op)
			// only notify completion when request waiting on same server and channel available
			// we also need to ensure `ToClientCh` is not nil, because if server restarts before this entry committed
			// log will be reloaded from persistent state and channel will be set to nil since it's non-serializable
			// if the source server happened to become leader again to commit this entry, it will pass first check and cause dead lock in Raft
			if op.From == kv.me && op.ToClientCh != nil {
				op.ToClientCh <- result
			}
		case killed := <-kv.killCh:
			if killed {
				break
			}
		}
	}
}

func (kv *KVServer) FailAllPendingRequests() {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	for k, v := range kv.pendingRequests {
		v.failCh <- true
		delete(kv.pendingRequests, k)
	}
}

func (kv *KVServer) FailConflictPendingRequests(cmd raft.ApplyMsg) {
	op := cmd.Command.(Op)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	info, ok := kv.pendingRequests[cmd.CommandIndex]
	if ok && info.requestId != op.RequestId() {
		info.failCh <- true
	}
	delete(kv.pendingRequests, cmd.CommandIndex)
}

func (kv *KVServer) ApplyOperation(op Op) string {
	result := ""
	// No lock need here because the only competing goroutine is read only
	seq, ok := kv.dedupTable[op.ClientId]
	if ok && op.SeqNumber == seq {
		return kv.valueTable[op.ClientId]
	}
	// Q: will there be op.SeqNumber < seq?
	// no, because the sequence numbers occurred at commit stage is non-decreasing, it can only have duplicate caused by leader crash
	// this is guaranteed by the fact that client will only increase sequence number when previous RPC has finished
	// which means all lower sequence numbers have been committed for at least once
	if op.Op == PutOp {
		kv.state[op.Key] = op.Value
	} else {
		value, ok := kv.state[op.Key]
		if !ok {
			value = ""
		}
		switch op.Op {
		case AppendOp:
			kv.state[op.Key] = value + op.Value
		case GetOp:
			result = value
		}
	}
	kv.rwLock.Lock()
	kv.dedupTable[op.ClientId] = op.SeqNumber
	kv.valueTable[op.ClientId] = result
	kv.rwLock.Unlock()
	return result
}

// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	kv.killCh <- true
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.killCh = make(chan bool, 10)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	kv.state = make(map[string]string)
	kv.dedupTable = make(map[int64]int32)
	kv.valueTable = make(map[int64]string)
	kv.pendingRequests = make(map[int]RequestInfo)
	go kv.OperationExecutor()

	return kv
}
