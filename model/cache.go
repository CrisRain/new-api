package model

import (
	"errors"
	"fmt"
	"one-api/common"
	"one-api/setting"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	// group -> model -> priority -> list of channels
	group2model2priority2channels map[string]map[string]map[int64][]*Channel
	// group -> model -> priority -> round robin index
	group2model2priority2roundRobinIndex map[string]map[string]map[int64]*uint64
	// channel id -> channel
	channelsIDM map[int]*Channel
	// sync lock
	channelSyncLock sync.RWMutex
)

func InitChannelCache() {
	if !common.MemoryCacheEnabled {
		return
	}
	newChannelsIDM := make(map[int]*Channel)
	newGroup2model2priority2channels := make(map[string]map[string]map[int64][]*Channel)
	newGroup2model2priority2roundRobinIndex := make(map[string]map[string]map[int64]*uint64)

	var channels []*Channel
	DB.Where("status = ?", common.ChannelStatusEnabled).Find(&channels)
	for _, channel := range channels {
		newChannelsIDM[channel.Id] = channel
		groups := strings.Split(channel.Group, ",")
		models := strings.Split(channel.Models, ",")
		for _, group := range groups {
			if _, ok := newGroup2model2priority2channels[group]; !ok {
				newGroup2model2priority2channels[group] = make(map[string]map[int64][]*Channel)
				newGroup2model2priority2roundRobinIndex[group] = make(map[string]map[int64]*uint64)
			}
			for _, model := range models {
				if _, ok := newGroup2model2priority2channels[group][model]; !ok {
					newGroup2model2priority2channels[group][model] = make(map[int64][]*Channel)
					newGroup2model2priority2roundRobinIndex[group][model] = make(map[int64]*uint64)
				}
				priority := channel.GetPriority()
				if _, ok := newGroup2model2priority2channels[group][model][priority]; !ok {
					newGroup2model2priority2channels[group][model][priority] = make([]*Channel, 0)
					var i uint64 = 0
					newGroup2model2priority2roundRobinIndex[group][model][priority] = &i
				}
				// Append channel based on its weight
				weight := channel.GetWeight()
				if weight <= 0 {
					weight = 1
				}
				for i := 0; i < weight; i++ {
					newGroup2model2priority2channels[group][model][priority] = append(newGroup2model2priority2channels[group][model][priority], channel)
				}
			}
		}
	}

	channelSyncLock.Lock()
	channelsIDM = newChannelsIDM
	group2model2priority2channels = newGroup2model2priority2channels
	group2model2priority2roundRobinIndex = newGroup2model2priority2roundRobinIndex
	channelSyncLock.Unlock()
	common.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

func CacheGetRandomSatisfiedChannel(c *gin.Context, group string, model string, retry int) (*Channel, string, error) {
	var channel *Channel
	var err error
	selectGroup := group
	if group == "auto" {
		if len(setting.AutoGroups) == 0 {
			return nil, selectGroup, errors.New("auto groups is not enabled")
		}
		for _, autoGroup := range setting.AutoGroups {
			if common.DebugEnabled {
				println("autoGroup:", autoGroup)
			}
			channel, _ = getRandomSatisfiedChannel(autoGroup, model, retry)
			if channel == nil {
				continue
			} else {
				c.Set("auto_group", autoGroup)
				selectGroup = autoGroup
				if common.DebugEnabled {
					println("selectGroup:", selectGroup)
				}
				break
			}
		}
	} else {
		channel, err = getRandomSatisfiedChannel(group, model, retry)
		if err != nil {
			return nil, group, err
		}
	}
	if channel == nil {
		return nil, group, errors.New("channel not found")
	}
	return channel, selectGroup, nil
}

func getRandomSatisfiedChannel(group string, model string, retry int) (*Channel, error) {
	if strings.HasPrefix(model, "gpt-4-gizmo") {
		model = "gpt-4-gizmo-*"
	}
	if strings.HasPrefix(model, "gpt-4o-gizmo") {
		model = "gpt-4o-gizmo-*"
	}
	if !common.MemoryCacheEnabled {
		return GetRandomSatisfiedChannel(group, model, retry)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	priority2channels, ok := group2model2priority2channels[group][model]
	if !ok {
		return nil, errors.New("channel not found for group and model")
	}

	var priorities []int64
	for p := range priority2channels {
		priorities = append(priorities, p)
	}
	sort.Slice(priorities, func(i, j int) bool {
		return priorities[i] > priorities[j]
	})

	if len(priorities) == 0 {
		return nil, errors.New("no available priorities")
	}
	if retry >= len(priorities) {
		retry = len(priorities) - 1
	}
	targetPriority := priorities[retry]
	weightedChannels := priority2channels[targetPriority]
	if len(weightedChannels) == 0 {
		return nil, errors.New("no channels for the selected priority")
	}

	// Get the round-robin index for the current priority
	indexPtr := group2model2priority2roundRobinIndex[group][model][targetPriority]
	// Use atomic operation to get the next index in a thread-safe way
	nextIndex := atomic.AddUint64(indexPtr, 1) - 1
	// Get the channel using weighted round-robin
	channel := weightedChannels[int(nextIndex%uint64(len(weightedChannels)))]
	return channel, nil
}

func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, errors.New(fmt.Sprintf("当前渠道# %d，已不存在", id))
	}
	return c, nil
}

func CacheUpdateChannelStatus(id int, status int) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	if channel, ok := channelsIDM[id]; ok {
		channel.Status = status
	}
}
