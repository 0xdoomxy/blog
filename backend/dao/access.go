package dao

import (
	"blog/dao/db"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/streadway/amqp"
	"gorm.io/gorm"
)

func init() {
	db.GetMysql().AutoMigrate(&Access{})
	// init thr rabbit mq channel
	var err error
	var channel *amqp.Channel
	var articleExchange = viper.GetString("rabbitmq.articleexchange")
	var accessQueue = viper.GetString("rabbitmq.accessqueue")

	channel, err = db.GetRabbitmqChannel()
	if err != nil {
		logrus.Fatal("create the rabbitmq channel failed:", err.Error())
	}
	// init the rabbit mq  when the queue cant be created
	_, err = channel.QueueDeclare(accessQueue, true, false, false, false, nil)
	if err != nil {
		logrus.Fatalf("create the rabbitmq queue %s failed: %s", accessQueue, err.Error())
	}
	err = channel.ExchangeDeclare(articleExchange, amqp.ExchangeTopic, true, false, false, false, nil)
	if err != nil {
		logrus.Fatalf("create the rabbitmq exchange %s failed: %s", articleExchange, err.Error())
	}
	err = channel.QueueBind(accessQueue, accessQueue, articleExchange, false, nil)
	if err != nil {
		logrus.Fatalf("bind the rabbitmq queue %s to exchange %s failed: %s", accessQueue, articleExchange, err.Error())
	}
	// bind channel to accesss struct
	accessDao.mqChannel = channel
	accessDao.exchange = articleExchange
	accessDao.routingKey = accessQueue
	accessDao.cacheKey = viper.GetString("access.cachekeyPrefix")
	go func() {
		// flush the access cache to rabbitmq
		ticker := time.NewTicker(2 * time.Minute)
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		for {
			select {
			case <-ticker.C:
				accessDao.mutex.Lock()
				var err error
				var msg = amqp.Publishing{}
				var body []byte
				for articleId, num := range accessDao.delayMap {
					delete(accessDao.delayMap, articleId)
					body, err = json.Marshal(&Access{ArticleID: articleId, AccessNum: num})
					if err != nil {
						logrus.Error("marshal the article access failed:", err.Error())
						continue
					}
					msg.Body = body
					err = accessDao.mqChannel.Publish(accessDao.exchange, accessDao.routingKey, false, false, msg)
					if err != nil {
						logrus.Error("publish the article access to rabbitmq failed:", err.Error())
					}
				}
				accessDao.mutex.Unlock()
			case <-sigs:
				accessDao.mutex.Lock()
				var err error
				var msg = amqp.Publishing{}
				var body []byte
				for articleId, num := range accessDao.delayMap {
					delete(accessDao.delayMap, articleId)
					body, err = json.Marshal(&Access{ArticleID: articleId, AccessNum: num})
					if err != nil {
						logrus.Error("marshal the article access failed:", err.Error())
						continue
					}
					msg.Body = body
					err = accessDao.mqChannel.Publish(accessDao.exchange, accessDao.routingKey, false, false, msg)
					if err != nil {
						logrus.Error("publish the article access to rabbitmq failed:", err.Error())
					}
				}
				accessDao.mutex.Unlock()
			default:
				time.Sleep(1 * time.Second)
			}
		}
	}()
}

type access struct {
	_          [0]func() //disallow ==
	delayMap   map[uint]uint
	mutex      sync.Mutex
	mqChannel  *amqp.Channel
	exchange   string
	routingKey string
	cacheKey   string
}

var accessDao = &access{
	delayMap: make(map[uint]uint),
	mutex:    sync.Mutex{},
}

/*
*

	访问表

*
*/
type Access struct {
	ArticleID uint `gorm:"primaryKey"`
	AccessNum uint `gorm:"not null"`
}

func (a *Access) MarshalBinary() ([]byte, error) {
	return json.Marshal(a)
}

func (a *Access) UnmarshalBinary(data []byte) error {
	return json.Unmarshal(data, a)
}

func GetAccess() *access {
	return accessDao
}

func (a *access) CreateAccess(ctx context.Context, access *Access) (err error) {
	err = db.GetMysql().WithContext(ctx).Model(&Access{}).Create(access).Error
	if err != nil {
		return
	}
	return
}

func (a *access) IncrementAccess(articleId uint, num int) {
	a.mutex.Lock()
	if _, ok := a.delayMap[articleId]; ok {
		a.delayMap[articleId] += uint(num)
	} else {
		a.delayMap[articleId] = uint(num)
	}
	a.mutex.Unlock()
	return
}

func (a *access) FindAccessById(ctx context.Context, id uint) (access Access, err error) {
	cache := db.GetRedis()
	key := fmt.Sprintf("%s_%d", a.cacheKey, id)
	err = cache.Get(ctx, key).Scan(&access)
	if err != redis.Nil {
		if err != nil {
			logrus.Errorf("get access %d from redis failed:%s", id, err.Error())
		}
		return
	}
	err = db.GetMysql().WithContext(ctx).Model(&Access{}).Where("article_id = ?", id).First(&access).Error
	if err != nil {
		logrus.Errorf("get access %d from mysql failed:%s", id, err.Error())
		return
	}
	cache.Set(ctx, key, access, time.Duration(viper.GetInt64("cache.cleaninterval"))*time.Millisecond)
	return
}

func (a *access) DeleteAccess(ctx context.Context, id uint) (err error) {
	cache := db.GetRedis()
	key := fmt.Sprintf("%s_%d", a.cacheKey, id)
	err = cache.Del(ctx, key).Err()
	if err != nil && err != redis.Nil {
		logrus.Errorf("delete the access %d from redis failed:%s", id, err.Error())
		return
	}
	err = db.GetMysql().WithContext(ctx).Model(&Access{}).Where("article_id = ?", id).Delete(&Access{}).Error
	if err != nil {
		logrus.Errorf(" delete the access %d from mysql failed:%s", id, err.Error())
	}
	return
}

func (a *access) FindMaxAccessByPage(ctx context.Context, page, size int) (articles []*Access, total int64, err error) {
	storage := db.GetMysql()
	err = storage.WithContext(ctx).Model(&Access{}).Count(&total).Error
	if err != nil {
		return
	}
	err = storage.WithContext(ctx).Model(&Access{}).Offset((page - 1) * size).Limit(size).Order("access_num desc").Find(&articles).Error
	return
}

func (a *access) IncrementAccessNumToDB(ctx context.Context, access Access) (err error) {
	cache := db.GetRedis()
	err = cache.Del(ctx, fmt.Sprintf("%s_%d", a.cacheKey, access.ArticleID)).Err()
	if err != nil && err != redis.Nil {
		logrus.Errorf("delete the access %d from redis failed:%s", access.ArticleID, err.Error())
		return
	}
	err = db.GetMysql().Model(&Access{}).Where("article_id = ?", access.ArticleID).Update("access_num", gorm.Expr("access_num + ?", access.AccessNum)).Error
	if err != nil {
		logrus.Errorf("increment access %v number to db error:%s", access, err.Error())
	}
	return
}
