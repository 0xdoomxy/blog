package service

import (
	"blog/dao"
	"blog/dao/db"
	"blog/middleware/hotkey"
	"context"
	"io"
	"mime/multipart"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var imageBucketName string

func init() {
	imageBucketName = viper.GetString("article.imagesbucketname")
	hotArticlePool, err := hotkey.NewHotkey(&hotkey.Option{
		HotKeyCnt:  1000,
		LocalCache: hotkey.NewLocalCache(1000),
		AutoCache:  true,
		CacheMs:    viper.GetInt("hotkey.cachems"),
	})
	if err != nil {
		logrus.Panic("create hot article pool failed:", err.Error())
	}
	articleService = &article{
		hotArticlePool: hotArticlePool,
	}
}

type article struct {
	hotArticlePool *hotkey.HotKeyWithCache
}

var articleService *article

func GetArticle() *article {
	return articleService
}

/*
*
上传文章相关的图片到阿里云oss存储
@param filename string 文件名
@param file multipart.File 文件
@return string 图片
@return error 错误
*/
func (a *article) UploadImage(filename string, file multipart.File) error {
	//将图片保存到阿里云oss存储
	return db.GetBucket(imageBucketName).PutObject(filename, file)
}

/*
*

	下载图片

*
*/
func (a *article) DownloadImage(filename string) (res []byte, err error) {
	var reader io.Reader
	reader, err = db.GetBucket(imageBucketName).GetObject(filename)
	if err != nil {
		logrus.Error("get image from oss failed:", err.Error())
		return nil, err
	}
	res, err = io.ReadAll(reader)
	if err != nil {
		logrus.Error("read image from reader failed:", err.Error())
		return nil, err
	}
	return res, nil
}

/*
*

	发布文章

*
*/
//TODO发布文章的时候，对应标签的文章数会加1
func (a *article) PublishArticle(ctx context.Context, article *dao.Article) (err error) {
	//文章dao
	articledao := dao.GetArticle()
	//访问dao
	accessdao := dao.GetAccess()
	//点赞dao
	likedao := dao.GetLike()
	err = articledao.CreateArticle(ctx, article)
	if err != nil {
		logrus.Errorf("create article %v failed: %s", article, err.Error())
		return err
	}
	defer func() {
		if err != nil {
			//如果文章创建失败，删除mysql中的文章
			articledao.DeleteArticle(ctx, article.ID)
		}
	}()
	//已经拿到了articleID
	//将文章内容和tags存储到es中
	err = articledao.BuildArticleSearch(ctx, article)
	if err != nil {
		logrus.Errorf("build article (%v) search failed: %s", article, err.Error())
		return
	}
	//创建所对应的点赞数
	err = likedao.CreateLike(ctx, &dao.Like{ArticleID: article.ID, LikeNum: 0})
	if err != nil {
		logrus.Errorf("article (%v) create like failed: %s", article, err.Error())
		return
	}
	defer func() {
		if err != nil {
			likedao.DeleteLike(ctx, article.ID)
		}
	}()
	//创建所对应的访问数
	err = accessdao.CreateAccess(ctx, &dao.Access{ArticleID: article.ID, AccessNum: 0})
	if err != nil {
		logrus.Errorf("article (%v) create access failed: %s", article, err.Error())
		return
	}
	defer func() {
		if err != nil {
			accessdao.DeleteAccess(ctx, article.ID)
		}
	}()
	// 所对应标签的文章数加1
	err = GetTag().IncrementArticleNumByName(ctx, article.Tags, 1)
	if err != nil {
		logrus.Errorf("article (%v) increment tag article num failed: %s", article, err.Error())
		return
	}
	return
}

type ArticleView struct {
	ID         uint      `json:"id"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	Tags       string    `json:"tags"`
	Creator    uint      `json:"creator"`
	CreateTime time.Time `json:"create_time"`
	AccessNum  uint      `json:"access_num"`
	LikeNum    uint      `json:"like_num"`
}

type ArticleViewByPage struct {
	Total    uint           `json:"total"`
	Articles []*ArticleView `json:"articles"`
}

/*
*

	尝试获取热点文章信息

*
*/
func (a *article) tryGet(artcileid uint) (view *ArticleView, ok bool) {
	var cache interface{}
	cache, ok = a.hotArticlePool.Get(strconv.Itoa(int(artcileid)))
	if !ok {
		return
	}
	view, ok = cache.(*ArticleView)
	return
}

/*
*

	尝试缓存热点文章,注意，这里面topk的信息是一段时间内的access，而不是总的

*
*/
func (a *article) addWithValue(articleid uint, view *ArticleView, incr uint) {
	a.hotArticlePool.AddWithValue(strconv.Itoa(int(articleid)), view, uint32(incr))
}

/*
*

	尝试调整热点文章,注意，这里面topk的信息是一段时间内的access，而不是总的

*
*/
func (a *article) add(articleid uint, incr uint) {
	a.hotArticlePool.Add(strconv.Itoa(int(articleid)), uint32(incr))
}

/*
*

	查询一个文章的全部信息

*
*/
func (a *article) FindArticle(ctx context.Context, articleid uint) (view *ArticleView, err error) {
	accessdao := dao.GetAccess()
	//如果是热点文章,直接返回缓存
	if view, ok := a.tryGet(articleid); ok {
		//当前topk中存在的话，直接追加保持该文章的热度
		go a.add(articleid, 1)
		//增加访问数
		go accessdao.IncrementAccess(articleid, 1)
		return view, nil
	}
	articledao := dao.GetArticle()
	likedao := dao.GetLike()
	var article dao.Article
	article, err = articledao.FindArticleById(ctx, articleid)
	if err != nil {
		logrus.Errorf("find article by id %d failed: %s", articleid, err.Error())
		return
	}
	var access dao.Access
	access, err = accessdao.FindAccessById(ctx, articleid)
	if err != nil {
		logrus.Errorf("find access by id %d failed: %s", articleid, err.Error())
	}
	var like dao.Like
	like, err = likedao.FindLikeById(ctx, articleid)
	if err != nil {
		logrus.Errorf("find like by id %d failed: %s", articleid, err.Error())
	}
	view = &ArticleView{
		ID:         article.ID,
		Title:      article.Title,
		Content:    article.Content,
		Tags:       article.Tags,
		Creator:    article.Creator,
		CreateTime: article.CreatedAt,
		AccessNum:  access.AccessNum,
		LikeNum:    like.LikeNum,
	}
	//增加访问数
	go accessdao.IncrementAccess(articleid, 1)
	//增加热点文章
	go a.addWithValue(articleid, view, 1)
	return
}

/*
*

	查询一个文章的概要信息
*
*/

func (a *article) FindArticlePatical(ctx context.Context, articleid uint) (view *ArticleView, err error) {
	articledao := dao.GetArticle()
	accessdao := dao.GetAccess()
	likedao := dao.GetLike()

	var article dao.Article
	article, err = articledao.FindArticlePaticalById(ctx, articleid)
	if err != nil {
		logrus.Errorf("find article by id %d failed: %s", articleid, err.Error())
		return
	}
	var access dao.Access
	access, err = accessdao.FindAccessById(ctx, articleid)
	if err != nil {
		logrus.Errorf("find access by id %d failed: %s", articleid, err.Error())
		return
	}
	var like dao.Like
	like, err = likedao.FindLikeById(ctx, articleid)
	if err != nil {
		logrus.Errorf("find like by id %d failed: %s", articleid, err.Error())
		return
	}
	view = &ArticleView{
		ID:         article.ID,
		Title:      article.Title,
		Creator:    article.Creator,
		CreateTime: article.CreatedAt,
		AccessNum:  access.AccessNum,
		LikeNum:    like.LikeNum,
		Tags:       article.Tags,
	}
	return
}
func (a *article) SearchArticleByPage(ctx context.Context, keyword string, page int, pagesize int) (view *ArticleViewByPage, err error) {
	articledao := dao.GetArticle()
	var targetIds []uint64
	var total uint
	targetIds, total, err = articledao.SearchArticleByPage(ctx, keyword, page, pagesize)
	if err != nil {
		logrus.Errorf("search article failed: %s", err.Error())
		return
	}
	onceError := sync.Once{}
	wg := sync.WaitGroup{}
	wg.Add(len(targetIds))
	articleViews := make([]*ArticleView, 0, len(targetIds))
	for _, id := range targetIds {
		go func(id uint) {
			defer wg.Done()
			if err != nil {
				return
			}
			var view *ArticleView
			var tmpError error
			view, tmpError = a.FindArticlePatical(ctx, id)
			if tmpError != nil {
				logrus.Errorf("find article by id %d failed: %s", id, tmpError.Error())
				onceError.Do(func() {
					err = tmpError
				})
				return
			}
			articleViews = append(articleViews, view)
		}(uint(id))
	}
	wg.Wait()
	if err != nil {
		return
	}
	view = &ArticleViewByPage{
		Total:    total,
		Articles: articleViews,
	}
	return
}

/*
*

	通过accessNum查找文章

*
*/
func (a *article) FindArticleByAccessNum(ctx context.Context, page int, pagesize int) (view *ArticleViewByPage, err error) {
	articledao := dao.GetArticle()
	accessdao := dao.GetAccess()
	likedao := dao.GetLike()
	var total int64
	var access []*dao.Access
	access, total, err = accessdao.FindMaxAccessByPage(ctx, page, pagesize)
	if err != nil {
		logrus.Errorf("find article by access num failed: %s", err.Error())
		return
	}
	//初始化结果，并赋予访问数和文章主键
	articles := make([]*ArticleView, len(access))
	for i, a := range access {
		articles[i] = &ArticleView{
			ID:        a.ArticleID,
			AccessNum: a.AccessNum,
		}
	}
	onceError := sync.Once{}
	wg := sync.WaitGroup{}
	wg.Add(len(access))
	for i, _ := range access {
		go func(index int) {
			defer wg.Done()
			if err != nil {
				return
			}
			var articleView dao.Article
			var tmpError error
			articleView, tmpError = articledao.FindArticlePaticalById(ctx, articles[index].ID)
			if tmpError != nil {
				logrus.Errorf("find article by id %d failed: %s", articles[index].ID, tmpError.Error())
				onceError.Do(func() {
					err = tmpError
				})
				return
			}
			var likeView dao.Like
			likeView, tmpError = likedao.FindLikeById(ctx, articles[index].ID)
			if tmpError != nil {
				logrus.Errorf("find like by id %d failed: %s", articles[index].ID, tmpError.Error())
				onceError.Do(func() {
					err = tmpError
				})
				return
			}
			articles[index].LikeNum = likeView.LikeNum
			articles[index].Title = articleView.Title
			articles[index].CreateTime = articleView.CreatedAt
			articles[index].Creator = articleView.Creator
			articles[index].Tags = articleView.Tags
		}(i)
	}
	wg.Wait()
	view = &ArticleViewByPage{
		Total:    uint(total),
		Articles: articles,
	}
	return
}

/*
*
根据创建时间查找文章
*
*/
func (a *article) FindArticlePaticalByCreateTime(ctx context.Context, page int, pagesize int) (view *ArticleViewByPage, err error) {
	var accessDao = dao.GetAccess()
	var likeDao = dao.GetLike()
	var result []*dao.Article
	var total int64
	result, total, err = dao.GetArticle().FindArticlePaticalByCreateTime(ctx, page, pagesize)
	if err != nil {
		logrus.Errorf("find article by create time failed: %s", err.Error())
		return
	}
	articleViews := make([]*ArticleView, len(result))
	for i := 0; i < len(articleViews); i++ {
		articleViews[i] = &ArticleView{
			ID:         result[i].ID,
			Title:      result[i].Title,
			Creator:    result[i].Creator,
			CreateTime: result[i].CreatedAt,
			Tags:       result[i].Tags,
		}
	}
	wg := sync.WaitGroup{}
	wg.Add(len(articleViews))
	onceError := sync.Once{}
	for i, _ := range articleViews {
		go func(id uint) {
			defer wg.Done()
			if err != nil {
				return
			}
			accessResult, tmpErr := accessDao.FindAccessById(ctx, id)
			if tmpErr != nil {
				onceError.Do(func() {
					err = tmpErr
				})
				return
			}
			articleViews[i].AccessNum = accessResult.AccessNum
			likeResult, tmpErr := likeDao.FindLikeById(ctx, id)
			if tmpErr != nil {
				onceError.Do(func() {
					err = tmpErr
				})
				return
			}
			articleViews[i].LikeNum = likeResult.LikeNum
		}(articleViews[i].ID)
	}
	wg.Wait()
	view = &ArticleViewByPage{
		Total:    uint(total),
		Articles: articleViews,
	}
	return

}
