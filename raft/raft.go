package raft

import (
	"math/rand"
	"sync"
	"time"

	"bytes"
	"encoding/gob"
	"fmt"
	"github.com/sunhay/scratchpad/golang/mit-6.824-2017/src/labrpc"
)

// import "bytes"
// import "encoding/gob"

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make().
//
type ApplyMsg struct {
	Index       int
	Command     interface{}
	UseSnapshot bool   // ignore for lab2; only used in lab3
	Snapshot    []byte // ignore for lab2; only used in lab3
}

type ServerState string

const (
	Follower  ServerState = "Follower"
	Candidate             = "Candidate"
	Leader                = "Leader"
)

const HeartBeatInterval = 100 * time.Millisecond
const CommitApplyIdleCheckInterval = 100 * time.Millisecond
const LeaderPeerTickInterval = 10 * time.Millisecond

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	sync.Mutex // Lock to protect shared access to this peer's state

	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state

	// General state
	id               string
	me               int // this peer's index into peers[]
	state            ServerState
	isDecommissioned bool

	// Election state
	currentTerm int
	votedFor    string // Id of candidate that has voted for, this term. Empty string if no vote has been cast.
	leaderID    string

	// Log state
	log         []LogEntry
	commitIndex int
	lastApplied int

	// Leader state
	nextIndex      []int // For each peer, index of next log entry to send that server
	matchIndex     []int // For each peer, index of highest entry known log entry known to be replicated on peer
	sendAppendChan []chan struct{}

	// Liveness state
	lastHeartBeat time.Time // When this node last received a heartbeat message from the Leader
}

// RaftPersistence is persisted to the `persister`, and contains all necessary data to restart a failed node
type RaftPersistence struct {
	CurrentTerm int
	Log         []LogEntry
	VotedFor    string
}

// GetState return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {
	rf.Lock()
	defer rf.Unlock()
	return rf.currentTerm, rf.state == Leader
}

func (rf *Raft) getLastEntryInfo() (int, int) {
	if len(rf.log) > 0 {
		entry := rf.log[len(rf.log)-1]
		return entry.Index, entry.Term
	}
	return 0, 0
}

func (rf *Raft) transitionToCandidate() {
	rf.state = Candidate
	// Increment currentTerm and vote for self
	rf.currentTerm++
	rf.votedFor = rf.id
}

func (rf *Raft) transitionToFollower(newTerm int) {
	rf.state = Follower
	rf.currentTerm = newTerm
	rf.votedFor = ""
}

type LogEntry struct {
	Index   int
	Term    int
	Command interface{}
}

func (entry LogEntry) String() string {
	return fmt.Sprintf("LogEntry(Index: %d, Term: %d, Command: %d)", entry.Index, entry.Term, entry.Command)
}

// --- RequestVote RPC ---

// RequestVoteArgs - RPC arguments
type RequestVoteArgs struct {
	Term         int
	CandidateID  string
	LastLogIndex int
	LastLogTerm  int
}

// RequestVoteReply - RPC response
type RequestVoteReply struct {
	Term        int
	VoteGranted bool
	Id          string
}

func (reply *RequestVoteReply) VoteCount() int {
	if reply.VoteGranted {
		return 1
	}
	return 0
}

// RequestVote - RPC function
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.Lock()
	defer rf.Unlock()

	lastIndex, lastTerm := rf.getLastEntryInfo()
	logUpToDate := func() bool {
		if lastTerm == args.LastLogTerm {
			return lastIndex <= args.LastLogIndex
		}
		return lastTerm < args.LastLogTerm
	}()

	reply.Term = rf.currentTerm
	reply.Id = rf.id

	if args.Term < rf.currentTerm {
		reply.VoteGranted = false
	} else if args.Term >= rf.currentTerm && logUpToDate {
		rf.transitionToFollower(args.Term)
		rf.votedFor = args.CandidateID
		reply.VoteGranted = true
	} else if (rf.votedFor == "" || args.CandidateID == rf.votedFor) && logUpToDate {
		rf.votedFor = args.CandidateID
		reply.VoteGranted = true
	}

	rf.persist()
	RaftInfo("Vote requested for: %s on term: %d. Log up-to-date? %v. Vote granted? %v", rf, args.CandidateID, args.Term, logUpToDate, reply.VoteGranted)
}

func (rf *Raft) sendRequestVote(server int, voteChan chan int, args *RequestVoteArgs, reply *RequestVoteReply) {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	if voteChan <- server; !ok {
		rf.Lock()
		defer rf.Unlock()
		RaftDebug("Communication error: RequestVote() RPC failed", rf)
	}
}

// --- AppendEntries RPC ---

// AppendEntriesArgs - RPC arguments
type AppendEntriesArgs struct {
	Term             int
	LeaderID         string
	PreviousLogIndex int
	PreviousLogTerm  int
	LogEntries       []LogEntry
	LeaderCommit     int
}

// AppendEntriesReply - RPC response
type AppendEntriesReply struct {
	Term                int
	Success             bool
	ConflictingLogTerm  int // Term of the conflicting entry, if any
	ConflictingLogIndex int // First index of the log for the above conflicting term
}

// AppendEntries - RPC function
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.Lock()
	defer rf.Unlock()

	RaftDebug("Request from %s, w/ %d entries. Prev:[Index %d, Term %d]", rf, args.LeaderID, len(args.LogEntries), args.PreviousLogIndex, args.PreviousLogTerm)

	reply.Term = rf.currentTerm
	if args.Term < rf.currentTerm {
		reply.Success = false
		return
	} else if args.Term >= rf.currentTerm {
		rf.transitionToFollower(args.Term)
		rf.leaderID = args.LeaderID
	}

	if rf.leaderID == args.LeaderID {
		rf.lastHeartBeat = time.Now()
	}

	// Try to find supplied previous log entry match in our log
	prevLogIndex := -1
	for i, v := range rf.log {
		if v.Index == args.PreviousLogIndex {
			if v.Term == args.PreviousLogTerm {
				prevLogIndex = i
				break
			} else {
				reply.ConflictingLogTerm = v.Term
			}
		}
	}

	isBeginningOfLog := args.PreviousLogIndex == 0 && args.PreviousLogTerm == 0
	if prevLogIndex >= 0 || isBeginningOfLog {
		if len(args.LogEntries) > 0 {
			RaftInfo("Appending %d entries from %s", rf, len(args.LogEntries), args.LeaderID)
		}

		// Remove any inconsistent logs and find the index of the last consistent entry from the leader
		entriesIndex := 0
		for i := prevLogIndex + 1; i < len(rf.log); i++ {
			entryConsistent := func() bool {
				localEntry, leadersEntry := rf.log[i], args.LogEntries[entriesIndex]
				return localEntry.Index == leadersEntry.Index && localEntry.Term == leadersEntry.Term
			}
			if entriesIndex >= len(args.LogEntries) || !entryConsistent() {
				// Additional entries must be inconsistent, so let's delete them from our local log
				rf.log = rf.log[:i]
				break
			} else {
				entriesIndex++
			}
		}

		// Append all entries that are not already in our log
		if entriesIndex < len(args.LogEntries) {
			rf.log = append(rf.log, args.LogEntries[entriesIndex:]...)
		}

		// Update the commit index
		if args.LeaderCommit > rf.commitIndex {
			latestLogIndex := rf.log[len(rf.log)-1].Index
			if args.LeaderCommit < latestLogIndex {
				rf.commitIndex = args.LeaderCommit
			} else {
				rf.commitIndex = latestLogIndex
			}
		}
		reply.Success = true
	} else {
		// §5.3: When rejecting an AppendEntries request, the follower can include the term of the
		//	 	 conflicting entry and the first index it stores for that term.

		// If there's no entry with `args.PreviousLogIndex` in our log. Set conflicting term to that of last log entry
		if reply.ConflictingLogTerm == 0 {
			reply.ConflictingLogTerm = rf.log[len(rf.log)-1].Term
		}

		for _, v := range rf.log { // Find first log index for the conflicting term
			if v.Term == reply.ConflictingLogTerm {
				reply.ConflictingLogIndex = v.Index
				break
			}
		}

		reply.Success = false
	}
	rf.persist()
}

func (rf *Raft) sendAppendEntries(peerIndex int, sendAppendChan chan struct{}) {
	rf.Lock()

	if rf.state != Leader || rf.isDecommissioned {
		rf.Unlock()
		return
	}

	var entries []LogEntry = []LogEntry{}
	var prevLogIndex, prevLogTerm int = 0, 0

	peerId := string(rune(peerIndex + 'A'))
	lastLogIndex, _ := rf.getLastEntryInfo()

	if lastLogIndex > 0 && lastLogIndex >= rf.nextIndex[peerIndex] {
		for i, v := range rf.log { // Need to send logs beginning from index `rf.nextIndex[peerIndex]`
			if v.Index == rf.nextIndex[peerIndex] {
				if i > 0 {
					lastEntry := rf.log[i-1]
					prevLogIndex, prevLogTerm = lastEntry.Index, lastEntry.Term
				}
				entries = make([]LogEntry, len(rf.log)-i)
				copy(entries, rf.log[i:])
				break
			}
		}
		RaftDebug("Sending log %d entries to %s", rf, len(entries), peerId)
	} else { // We're just going to send a heartbeat
		if len(rf.log) > 0 {
			lastEntry := rf.log[len(rf.log)-1]
			prevLogIndex, prevLogTerm = lastEntry.Index, lastEntry.Term
		}
	}

	reply := AppendEntriesReply{}
	args := AppendEntriesArgs{
		Term:             rf.currentTerm,
		LeaderID:         rf.id,
		PreviousLogIndex: prevLogIndex,
		PreviousLogTerm:  prevLogTerm,
		LogEntries:       entries,
		LeaderCommit:     rf.commitIndex,
	}
	rf.Unlock()

	ok := rf.peers[peerIndex].Call("Raft.AppendEntries", &args, &reply)

	rf.Lock()
	defer rf.Unlock()

	if !ok {
		RaftDebug("Communication error: AppendEntries() RPC failed", rf)
	} else if reply.Success {
		if len(entries) > 0 {
			RaftInfo("Appended %d entries to %s's log", rf, len(entries), peerId)
			lastReplicated := entries[len(entries)-1]
			rf.matchIndex[peerIndex] = lastReplicated.Index
			rf.nextIndex[peerIndex] = lastReplicated.Index + 1
			rf.updateCommitIndex()
		} else {
			RaftDebug("Successful heartbeat from %s", rf, peerId)
		}
	} else {
		if reply.Term > rf.currentTerm {
			RaftInfo("Switching to follower as %s's term is %d", rf, peerId, reply.Term)
			rf.transitionToFollower(reply.Term)
		} else {
			RaftInfo("Log deviation on %s @ T: %d. nextIndex: %d, args.Prev[I: %d, T: %d], FirstConflictEntry[I: %d, T: %d]", rf, peerId, reply.Term, rf.nextIndex[peerIndex], args.PreviousLogIndex, args.PreviousLogTerm, reply.ConflictingLogIndex, reply.ConflictingLogTerm)
			// Log deviation, we should go back to `ConflictingLogIndex - 1`, lowest value for nextIndex[peerIndex] is 1.
			rf.nextIndex[peerIndex] = Max(reply.ConflictingLogIndex-1, 1)
			sendAppendChan <- struct{}{} // Signals to leader-peer process that appends need to occur
		}
	}
	rf.persist()
}

func (rf *Raft) updateCommitIndex() {
	// §5.3/5.4: If there exists an N such that N > commitIndex, a majority of matchIndex[i] ≥ N, and log[N].term == currentTerm: set commitIndex = N
	for i := len(rf.log) - 1; i >= 0; i-- {
		if v := rf.log[i]; v.Term == rf.currentTerm && v.Index > rf.commitIndex {
			replicationCount := 1
			for j := range rf.peers {
				if j != rf.me && rf.matchIndex[j] >= v.Index {
					if replicationCount++; replicationCount > len(rf.peers)/2 { // Check to see if majority of nodes have replicated this
						RaftInfo("Updating commit index [%d -> %d] as replication factor is at least: %d/%d", rf, rf.commitIndex, v.Index, replicationCount, len(rf.peers))
						rf.commitIndex = v.Index // Set index of this entry as new commit index
						break
					}
				}
			}
		} else {
			break
		}
	}
}

func (rf *Raft) startLocalApplyProcess(applyChan chan ApplyMsg) {
	rf.Lock()
	RaftInfo("Starting commit process - Last log applied: %d", rf, rf.lastApplied)
	rf.Unlock()

	for {
		rf.Lock()

		if rf.commitIndex >= 0 && rf.commitIndex > rf.lastApplied {
			entries := make([]LogEntry, rf.commitIndex-rf.lastApplied)
			copy(entries, rf.log[rf.lastApplied:rf.commitIndex])
			RaftInfo("Locally applying %d log entries. lastApplied: %d. commitIndex: %d", rf, len(entries), rf.lastApplied, rf.commitIndex)
			rf.Unlock()

			for _, v := range entries { // Hold no locks so that slow local applies don't deadlock the system
				RaftDebug("Locally applying log: %s", rf, v)
				applyChan <- ApplyMsg{Index: v.Index, Command: v.Command}
			}

			rf.Lock()
			rf.lastApplied += len(entries)
			rf.Unlock()
		} else {
			rf.Unlock()
			<-time.After(CommitApplyIdleCheckInterval)
		}
	}
}

func (rf *Raft) startElectionProcess() {
	electionTimeout := func() time.Duration { // Randomized timeouts between [500, 600)-ms
		return (500 + time.Duration(rand.Intn(100))) * time.Millisecond
	}

	currentTimeout := electionTimeout()
	currentTime := <-time.After(currentTimeout)

	rf.Lock()
	defer rf.Unlock()
	if !rf.isDecommissioned {
		// Start election process if we're not a leader and the haven't recieved a heartbeat for `electionTimeout`
		if rf.state != Leader && currentTime.Sub(rf.lastHeartBeat) >= currentTimeout {
			RaftInfo("Election timer timed out. Timeout: %fs", rf, currentTimeout.Seconds())
			go rf.beginElection()
		}
		go rf.startElectionProcess()
	}
}

func (rf *Raft) beginElection() {
	rf.Lock()

	rf.transitionToCandidate()
	RaftInfo("Election started", rf)

	// Request votes from peers
	lastIndex, lastTerm := rf.getLastEntryInfo()
	args := RequestVoteArgs{
		Term:         rf.currentTerm,
		CandidateID:  rf.id,
		LastLogTerm:  lastTerm,
		LastLogIndex: lastIndex,
	}
	replies := make([]RequestVoteReply, len(rf.peers))
	voteChan := make(chan int, len(rf.peers))
	for i := range rf.peers {
		if i != rf.me {
			go rf.sendRequestVote(i, voteChan, &args, &replies[i])
		}
	}
	rf.persist()
	rf.Unlock()

	// Count votes from peers as they come in
	votes := 1
	for i := 0; i < len(replies); i++ {
		reply := replies[<-voteChan]
		rf.Lock()

		// §5.1: If RPC request or response contains term T > currentTerm: set currentTerm = T, convert to follower
		if reply.Term > rf.currentTerm {
			RaftInfo("Switching to follower as %s's term is %d", rf, reply.Id, reply.Term)
			rf.transitionToFollower(reply.Term)
			rf.persist()
			rf.Unlock()
			return
		}

		if votes += reply.VoteCount(); votes > len(replies)/2 { // Has majority vote
			// Ensure that we're still a candidate and that another election did not interrupt
			if rf.state == Candidate && args.Term == rf.currentTerm {
				RaftInfo("Election won. Vote: %d/%d", rf, votes, len(rf.peers))
				go rf.promoteToLeader()
			} else {
				RaftInfo("Election for term %d was interrupted", rf, args.Term)
			}
			rf.persist()
			rf.Unlock()
			return
		}
		rf.Unlock()
	}
}

func (rf *Raft) promoteToLeader() {
	rf.Lock()
	defer rf.Unlock()

	rf.state = Leader
	rf.leaderID = rf.id

	rf.nextIndex = make([]int, len(rf.peers))
	rf.matchIndex = make([]int, len(rf.peers))
	rf.sendAppendChan = make([]chan struct{}, len(rf.peers))

	for i := range rf.peers {
		if i != rf.me {
			rf.nextIndex[i] = len(rf.log) + 1 // Should be initialized to leader's last log index + 1
			rf.matchIndex[i] = 0              // Index of highest log entry known to be replicated on server
			rf.sendAppendChan[i] = make(chan struct{}, 1)

			// Start routines for each peer which will be used to monitor and send log entries
			go rf.startLeaderPeerProcess(i, rf.sendAppendChan[i])
		}
	}
}

func (rf *Raft) startLeaderPeerProcess(peerIndex int, sendAppendChan chan struct{}) {
	ticker := time.NewTicker(LeaderPeerTickInterval)

	// Initial heartbeat
	rf.sendAppendEntries(peerIndex, sendAppendChan)
	lastEntrySent := time.Now()

	for {
		rf.Lock()
		if rf.state != Leader || rf.isDecommissioned {
			ticker.Stop()
			rf.Unlock()
			break
		}
		rf.Unlock()

		select {
		case <-sendAppendChan: // Signal that we should send a new append to this peer
			lastEntrySent = time.Now()
			rf.sendAppendEntries(peerIndex, sendAppendChan)
		case currentTime := <-ticker.C: // If traffic has been idle, we should send a heartbeat
			if currentTime.Sub(lastEntrySent) >= HeartBeatInterval {
				lastEntrySent = time.Now()
				rf.sendAppendEntries(peerIndex, sendAppendChan)
			}
		}
	}
}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	term, isLeader := rf.GetState()

	if !isLeader {
		return -1, term, isLeader
	}

	rf.Lock()
	defer rf.Unlock()

	nextIndex := func() int {
		if len(rf.log) > 0 {
			return len(rf.log) + 1
		}
		return 1
	}()

	rf.log = append(rf.log, LogEntry{Index: nextIndex, Term: rf.currentTerm, Command: command})
	RaftInfo("New entry appended to leader's log: %s", rf, rf.log[nextIndex-1])

	return nextIndex, term, isLeader
}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int, persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{
		peers:       peers,
		persister:   persister,
		me:          me,
		id:          string(rune(me + 'A')),
		state:       Follower,
		commitIndex: 0,
		lastApplied: 0,
	}

	RaftInfo("Node created", rf)

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	go rf.startElectionProcess()
	go rf.startLocalApplyProcess(applyCh)

	return rf
}

// --- Persistence ---

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
//
func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := gob.NewEncoder(w)

	e.Encode(RaftPersistence{CurrentTerm: rf.currentTerm, Log: rf.log, VotedFor: rf.votedFor})

	data := w.Bytes()
	RaftDebug("Persisting node data. Byte count: %d", rf, len(data))
	rf.persister.SaveRaftState(data)
}

//
// restore previously persisted state.
//
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 {
		return
	}

	r := bytes.NewBuffer(data)
	d := gob.NewDecoder(r)
	obj := RaftPersistence{}
	d.Decode(&obj)

	rf.votedFor = obj.VotedFor
	rf.currentTerm = obj.CurrentTerm
	rf.log = obj.Log
	RaftInfo("Loading persisted node data. Byte count: %d", rf, len(data))
}

func (rf *Raft) Kill() {
	rf.Lock()
	defer rf.Unlock()

	rf.isDecommissioned = true
	RaftInfo("Node killed", rf)
}
