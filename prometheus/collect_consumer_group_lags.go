package prometheus

import (
	"context"
	"math"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"go.uber.org/zap"

	"github.com/cloudhut/kminion/v2/minion"
)

type waterMark struct {
	TopicName     string
	PartitionID   int32
	LowWaterMark  int64
	HighWaterMark int64
}

func (e *Exporter) collectConsumerGroupLags(ctx context.Context, ch chan<- prometheus.Metric) bool {
	if !e.minionSvc.Cfg.ConsumerGroups.Enabled {
		return true
	}

	// Low Watermarks (at the moment they are not needed at all, they could be used to calculate the lag on partitions
	// that don't have any active offsets)
	lowWaterMarks, err := e.minionSvc.ListOffsetsCached(ctx, -2)
	if err != nil {
		e.logger.Error("failed to fetch low water marks", zap.Error(err))
		return false
	}
	// High Watermarks
	highWaterMarks, err := e.minionSvc.ListOffsetsCached(ctx, -1)
	if err != nil {
		e.logger.Error("failed to fetch low water marks", zap.Error(err))
		return false
	}
	waterMarksByTopic := e.waterMarksByTopic(lowWaterMarks, highWaterMarks)

	// We have two different options to get consumer group offsets - either via the AdminAPI or by consuming the
	// __consumer_offsets topic.
	if e.minionSvc.Cfg.ConsumerGroups.ScrapeMode == minion.ConsumerGroupScrapeModeAdminAPI {
		return e.collectConsumerGroupLagsAdminAPI(ctx, ch, waterMarksByTopic)
	} else {
		return e.collectConsumerGroupLagsOffsetTopic(ctx, ch, waterMarksByTopic)
	}
}

func (e *Exporter) collectConsumerGroupLagsOffsetTopic(_ context.Context, ch chan<- prometheus.Metric, marks map[string]map[int32]waterMark) bool {
	offsets := e.minionSvc.ListAllConsumerGroupOffsetsInternal()
	for groupName, group := range offsets {
		if !e.minionSvc.IsGroupAllowed(groupName) {
			continue
		}
		offsetCommits := 0

		for topicName, topic := range group {
			topicLag := float64(0)
			topicOffsetSum := float64(0)
			for partitionID, partition := range topic {
				childLogger := e.logger.With(
					zap.String("consumer_group", groupName),
					zap.String("topic_name", topicName),
					zap.Int32("partition_id", partitionID),
					zap.Int64("group_offset", partition.Value.Offset))

				topicMark, exists := marks[topicName]
				if !exists {
					childLogger.Warn("consumer group has committed offsets on a topic we don't have watermarks for")
					break // We can stop trying to find any other offsets for that topic so let's quit this loop
				}
				partitionMark, exists := topicMark[partitionID]
				if !exists {
					childLogger.Warn("consumer group has committed offsets on a partition we don't have watermarks for")
					continue
				}
				lag := float64(partitionMark.HighWaterMark - partition.Value.Offset)
				// Lag might be negative because we fetch group offsets after we get partition offsets. It's kinda a
				// race condition. Negative lags obviously do not make sense so use at least 0 as lag.
				lag = math.Max(0, lag)
				topicLag += lag
				topicOffsetSum += float64(partition.Value.Offset)

				// Offset commit count for this consumer group
				offsetCommits += partition.CommitCount

				if e.minionSvc.Cfg.ConsumerGroups.Granularity == minion.ConsumerGroupGranularityTopic {
					continue
				}
				ch <- prometheus.MustNewConstMetric(
					e.consumerGroupTopicPartitionLag,
					prometheus.GaugeValue,
					lag,
					groupName,
					topicName,
					strconv.Itoa(int(partitionID)),
				)
			}
			ch <- prometheus.MustNewConstMetric(
				e.consumerGroupTopicLag,
				prometheus.GaugeValue,
				topicLag,
				groupName,
				topicName,
			)
			ch <- prometheus.MustNewConstMetric(
				e.consumerGroupTopicOffsetSum,
				prometheus.GaugeValue,
				topicOffsetSum,
				groupName,
				topicName,
			)
		}

		ch <- prometheus.MustNewConstMetric(
			e.offsetCommits,
			prometheus.CounterValue,
			float64(offsetCommits),
			groupName,
		)
	}
	return true
}

func (e *Exporter) collectConsumerGroupLagsAdminAPI(ctx context.Context, ch chan<- prometheus.Metric, marks map[string]map[int32]waterMark) bool {
	isOk := true

	groupOffsets, err := e.minionSvc.ListAllConsumerGroupOffsetsAdminAPI(ctx)
	for groupName, offsetRes := range groupOffsets {
		if !e.minionSvc.IsGroupAllowed(groupName) {
			continue
		}

		err = kerr.ErrorForCode(offsetRes.ErrorCode)
		if err != nil {
			e.logger.Warn("failed to get offsets from consumer group, inner kafka error",
				zap.String("consumer_group", groupName),
				zap.Error(err))
			isOk = false
			continue
		}
		for _, topic := range offsetRes.Topics {
			topicLag := float64(0)
			topicOffsetSum := float64(0)
			for _, partition := range topic.Partitions {
				err := kerr.ErrorForCode(partition.ErrorCode)
				if err != nil {
					e.logger.Warn("failed to get consumer group offsets for a partition, inner kafka error",
						zap.String("consumer_group", groupName),
						zap.Error(err))
					isOk = false
					continue
				}

				childLogger := e.logger.With(
					zap.String("consumer_group", groupName),
					zap.String("topic_name", topic.Topic),
					zap.Int32("partition_id", partition.Partition),
					zap.Int64("group_offset", partition.Offset))
				topicMark, exists := marks[topic.Topic]
				if !exists {
					childLogger.Warn("consumer group has committed offsets on a topic we don't have watermarks for")
					isOk = false
					break // We can stop trying to find any other offsets for that topic so let's quit this loop
				}
				partitionMark, exists := topicMark[partition.Partition]
				if !exists {
					childLogger.Warn("consumer group has committed offsets on a partition we don't have watermarks for")
					isOk = false
					continue
				}
				lag := float64(partitionMark.HighWaterMark - partition.Offset)
				// Lag might be negative because we fetch group offsets after we get partition offsets. It's kinda a
				// race condition. Negative lags obviously do not make sense so use at least 0 as lag.
				lag = math.Max(0, lag)
				topicLag += lag
				topicOffsetSum += float64(partition.Offset)

				if e.minionSvc.Cfg.ConsumerGroups.Granularity == minion.ConsumerGroupGranularityTopic {
					continue
				}
				ch <- prometheus.MustNewConstMetric(
					e.consumerGroupTopicPartitionLag,
					prometheus.GaugeValue,
					lag,
					groupName,
					topic.Topic,
					strconv.Itoa(int(partition.Partition)),
				)
			}

			ch <- prometheus.MustNewConstMetric(
				e.consumerGroupTopicLag,
				prometheus.GaugeValue,
				topicLag,
				groupName,
				topic.Topic,
			)
			ch <- prometheus.MustNewConstMetric(
				e.consumerGroupTopicOffsetSum,
				prometheus.GaugeValue,
				topicOffsetSum,
				groupName,
				topic.Topic,
			)
		}
	}
	return isOk
}

func (e *Exporter) waterMarksByTopic(lowMarks kadm.ListedOffsets, highMarks kadm.ListedOffsets) map[string]map[int32]waterMark {
	type partitionID = int32
	type topicName = string
	waterMarks := make(map[topicName]map[partitionID]waterMark)

	for topic, lowMarksByPartitionID := range lowMarks {
		_, exists := waterMarks[topic]
		if !exists {
			waterMarks[topic] = make(map[partitionID]waterMark)
		}

		for _, lowOffset := range lowMarksByPartitionID {
			if lowOffset.Err != nil {
				e.logger.Debug("failed to get partition low water mark, inner kafka error",
					zap.String("topic_name", lowOffset.Topic),
					zap.Int32("partition_id", lowOffset.Partition),
					zap.Error(lowOffset.Err))
				continue
			}

			higOffset, exists := highMarks.Lookup(lowOffset.Topic, lowOffset.Partition)
			if !exists {
				e.logger.Error("got low water marks for a topic's partition but no high watermarks",
					zap.String("topic_name", lowOffset.Topic),
					zap.Int32("partition_id", lowOffset.Partition),
					zap.Int64("offset", lowOffset.Offset))
				delete(waterMarks, lowOffset.Topic)
				break // Topic watermarks are invalid -> delete & skip this topic
			}
			if higOffset.Err != nil {
				e.logger.Debug("failed to get partition low water mark, inner kafka error",
					zap.String("topic_name", lowOffset.Topic),
					zap.Int32("partition_id", lowOffset.Partition),
					zap.Error(lowOffset.Err))
				continue
			}

			waterMarks[lowOffset.Topic][lowOffset.Partition] = waterMark{
				TopicName:     lowOffset.Topic,
				PartitionID:   lowOffset.Partition,
				LowWaterMark:  lowOffset.Offset,
				HighWaterMark: higOffset.Offset,
			}
		}
	}

	return waterMarks
}
