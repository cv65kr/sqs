package sqsjobs

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"go.uber.org/zap"
)

const (
	// All - get all message attribute names
	All string = "All"

	// NonExistentQueue AWS error code
	NonExistentQueue string = "AWS.SimpleQueueService.NonExistentQueue"

	// consume all
	auto string = "deduced_by_rr"
)

func (c *Driver) listen(ctx context.Context) { //nolint:gocognit
	go func() {
		for {
			select {
			case <-c.pauseCh:
				c.log.Debug("sqs listener was stopped")
				return
			default:
				message, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
					QueueUrl:              c.queueURL,
					MaxNumberOfMessages:   10,
					AttributeNames:        []types.QueueAttributeName{types.QueueAttributeName(ApproximateReceiveCount)},
					MessageAttributeNames: []string{All},
					// The new value for the message's visibility timeout (in seconds). Values range: 0
					// to 43200. Maximum: 12 hours.
					VisibilityTimeout: c.visibilityTimeout,
					WaitTimeSeconds:   c.waitTime,
				})

				if err != nil { //nolint:nestif
					if oErr, ok := (err).(*smithy.OperationError); ok { //nolint:errorlint
						if rErr, ok := oErr.Err.(*http.ResponseError); ok { //nolint:errorlint
							if apiErr, ok := rErr.Err.(*smithy.GenericAPIError); ok { //nolint:errorlint
								// in case of NonExistentQueue - recreate the queue
								if apiErr.Code == NonExistentQueue {
									c.log.Error("receive message", zap.String("error code", apiErr.ErrorCode()), zap.String("message", apiErr.ErrorMessage()), zap.String("error fault", apiErr.ErrorFault().String()))
									_, err = c.client.CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: c.queue, Attributes: c.attributes, Tags: c.tags})
									if err != nil {
										c.log.Error("create queue", zap.Error(err))
									}
									// To successfully create a new queue, you must provide a
									// queue name that adheres to the limits related to the queues
									// (https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/limits-queues.html)
									// and is unique within the scope of your queues. After you create a queue, you
									// must wait at least one second after the queue is created to be able to use the <------------
									// queue. To get the queue URL, use the GetQueueUrl action. GetQueueUrl require
									time.Sleep(time.Second)
									continue
								}
							}
						}
					}

					c.log.Error("receive message", zap.Error(err))
					continue
				}

				for i := 0; i < len(message.Messages); i++ {
					c.cond.L.Lock()
					// lock when we hit the limit
					for atomic.LoadInt64(c.msgInFlight) >= int64(atomic.LoadInt32(c.msgInFlightLimit)) {
						c.log.Debug("prefetch limit was reached, waiting for the jobs to be processed", zap.Int64("current", atomic.LoadInt64(c.msgInFlight)), zap.Int32("limit", atomic.LoadInt32(c.msgInFlightLimit)))
						c.cond.Wait()
					}

					m := message.Messages[i]
					c.log.Debug("receive message", zap.Stringp("ID", m.MessageId))
					item, errUnp := c.fromMsg(&m)
					if errUnp != nil {
						_, errD := c.client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
							QueueUrl:      c.queueURL,
							ReceiptHandle: m.ReceiptHandle,
						})
						if errD != nil {
							c.log.Error("message unpack, failed to delete the message from the queue", zap.Error(errUnp), zap.Error(errD))
							c.cond.L.Unlock()
							continue
						}

						c.log.Error("message unpack", zap.Error(errUnp))
						c.cond.L.Unlock()
						continue
					}

					if item.Options.AutoAck {
						ctxT, cancel := context.WithTimeout(context.Background(), time.Minute)
						_, errD := c.client.DeleteMessage(ctxT, &sqs.DeleteMessageInput{
							QueueUrl:      c.queueURL,
							ReceiptHandle: m.ReceiptHandle,
						})
						if errD != nil {
							cancel()
							c.log.Error("message unpack, failed to delete the message from the queue", zap.Error(errUnp), zap.Error(errD))
							c.cond.L.Unlock()
							continue
						}
						cancel()

						c.log.Debug("auto ack is turned on, message acknowledged")
					}

					c.pq.Insert(item)
					// increase the current number of messages
					atomic.AddInt64(c.msgInFlight, 1)
					c.log.Debug("message pushed to the priority queue", zap.Int64("current", atomic.LoadInt64(c.msgInFlight)), zap.Int32("limit", atomic.LoadInt32(c.msgInFlightLimit)))
					c.cond.L.Unlock()
				}
			}
		}
	}()
}
