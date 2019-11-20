package asynq

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/go-redis/redis/v7"
)

// Redis keys
const (
	queuePrefix = "asynq:queues:"   // LIST
	allQueues   = "asynq:queues"    // SET
	scheduled   = "asynq:scheduled" // ZSET
	retry       = "asynq:retry"     // ZSET
	dead        = "asynq:dead"      // ZSET
)

var (
	errQueuePopTimeout = errors.New("blocking queue pop operation timed out")
	errSerializeTask   = errors.New("could not encode task message into json")
	errDeserializeTask = errors.New("could not decode task message from json")
)

// rdb encapsulates the interaction with redis server.
type rdb struct {
	client *redis.Client
}

func newRDB(client *redis.Client) *rdb {
	return &rdb{client}
}

// push enqueues the task to queue.
func (r *rdb) push(msg *taskMessage) error {
	bytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("could not encode task into JSON: %v", err)
	}
	qname := queuePrefix + msg.Queue
	err = r.client.SAdd(allQueues, qname).Err()
	if err != nil {
		return fmt.Errorf("command SADD %q %q failed: %v",
			allQueues, qname, err)
	}
	err = r.client.RPush(qname, string(bytes)).Err()
	if err != nil {
		return fmt.Errorf("command RPUSH %q %q failed: %v",
			qname, string(bytes), err)
	}
	return nil
}

// bpop blocks until there is a taskMessage available to be processed,
// returns immediately if there are already tasks waiting to be processed.
func (r *rdb) bpop(timeout time.Duration, keys ...string) (*taskMessage, error) {
	res, err := r.client.BLPop(timeout, keys...).Result()
	if err != nil {
		if err != redis.Nil {
			return nil, fmt.Errorf("command BLPOP %v %v failed: %v",
				timeout, keys, err)
		}
		return nil, errQueuePopTimeout
	}
	q, data := res[0], res[1]
	fmt.Printf("perform task %v from %s\n", data, q)
	var msg taskMessage
	err = json.Unmarshal([]byte(data), &msg)
	if err != nil {
		return nil, errDeserializeTask
	}
	return &msg, nil
}

// zadd adds the taskMessage to the specified zset (sorted set) with the given score.
func (r *rdb) zadd(zset string, zscore float64, msg *taskMessage) error {
	bytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("could not encode task into JSON: %v", err)
	}
	err = r.client.ZAdd(zset, &redis.Z{Member: string(bytes), Score: zscore}).Err()
	if err != nil {
		return fmt.Errorf("command ZADD %s %.1f %s failed: %v",
			zset, zscore, string(bytes), err)
	}
	return nil
}

func (r *rdb) zRangeByScore(key string, opt *redis.ZRangeBy) ([]*taskMessage, error) {
	jobs, err := r.client.ZRangeByScore(key, opt).Result()
	fmt.Printf("len(jobs) = %d\n", len(jobs))
	if err != nil {
		return nil, fmt.Errorf("command ZRANGEBYSCORE %s %v failed: %v", key, opt, err)
	}
	var msgs []*taskMessage
	for _, j := range jobs {
		fmt.Printf("[debug] j = %v\n", j)
		var msg taskMessage
		err = json.Unmarshal([]byte(j), &msg)
		if err != nil {
			log.Printf("[WARNING] could not unmarshal task data %s: %v\n", j, err)
			continue
		}
		msgs = append(msgs, &msg)
	}
	return msgs, nil
}

// move moves taskMessage from zfrom to the specified queue.
func (r *rdb) move(from string, msg *taskMessage) error {
	bytes, err := json.Marshal(msg)
	if err != nil {
		return errSerializeTask
	}
	if r.client.ZRem(from, string(bytes)).Val() > 0 {
		err = r.push(msg)
		if err != nil {
			log.Printf("[SERVERE ERROR] could not push task to queue %q: %v\n",
				msg.Queue, err)
			// TODO(hibiken): Handle this error properly.
			// Add back to zfrom?
			return fmt.Errorf("could not push task %v from %q: %v", msg, msg.Queue, err)
		}
	}
	return nil
}

const maxDeadTask = 100
const deadExpirationInDays = 90

// kill sends the taskMessage to "dead" set.
// It also trims the sorted set by timestamp and set size.
func (r *rdb) kill(msg *taskMessage) error {
	bytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("could not encode task into JSON: %v", err)
	}
	now := time.Now()
	pipe := r.client.Pipeline()
	pipe.ZAdd(dead, &redis.Z{Member: string(bytes), Score: float64(now.Unix())})
	limit := now.AddDate(0, 0, -deadExpirationInDays).Unix() // 90 days ago
	pipe.ZRemRangeByScore(dead, "-inf", strconv.Itoa(int(limit)))
	pipe.ZRemRangeByRank(dead, 0, -maxDeadTask) // trim the set to 100
	_, err = pipe.Exec()
	return err
}

// listQueues returns the list of all queues.
// NOTE: Add default to the slice if empty because
// BLPOP will error out if empty list is passed.
func (r *rdb) listQueues() []string {
	queues := r.client.SMembers(allQueues).Val()
	if len(queues) == 0 {
		queues = append(queues, queuePrefix+"default")
	}
	return queues
}
