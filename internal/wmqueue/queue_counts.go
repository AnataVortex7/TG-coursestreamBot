// internal/wmqueue/queue_counts.go
//
// GitHub Actions orchestrator (internal/ghworkers) साठी read-only counters.
// wmqueue.go मध्ये कोणताही बदल नाही — फक्त नवीन helper methods.
package wmqueue

// PendingCount — अजून कुठल्याही worker ने न घेतलेले jobs.
func (q *Queue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, id := range q.pending {
		if j, ok := q.jobs[id]; ok && j.Status == StatusPending {
			n++
		}
	}
	return n
}

// ProcessingCount — सध्या कुठल्यातरी worker वर चालू असलेले jobs.
func (q *Queue) ProcessingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, j := range q.jobs {
		if j.Status == StatusProcessing {
			n++
		}
	}
	return n
}
