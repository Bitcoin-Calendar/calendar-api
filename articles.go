package main

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// Editorial articles are independent of events; no body is added to the bot contract.
type Article struct {
	Metadata     map[string]interface{} `json:"metadata,omitempty" gorm:"serializer:json;type:text"`
	ArticleKey   string                 `json:"article_key" gorm:"primaryKey"`
	Slug         string                 `json:"slug"`
	Title        string                 `json:"title"`
	BodyMarkdown string                 `json:"body_markdown,omitempty"`
	Sources      []string               `json:"sources" gorm:"serializer:json;type:text"`
	Status       string                 `json:"-"`
	CreatedAt    string                 `json:"created_at"`
	UpdatedAt    string                 `json:"updated_at"`
	PublishedAt  *string                `json:"published_at"`
}
type ArticleTranslation struct {
	Locale string `json:"locale"`
	Slug   string `json:"slug"`
}
type ArticleDetail struct {
	Article
	RelatedEvents []Event              `json:"related_events"`
	Translations  []ArticleTranslation `json:"translations"`
}

var articleSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var articleColumns = map[string][]string{
	"articles":       {"article_key", "slug", "title", "body_markdown", "sources", "status", "created_at", "updated_at", "published_at"},
	"event_articles": {"event_id", "article_key", "is_primary", "sort_order"},
}

// Missing article schema is a capability failure; legacy event routes still work.
func articleSchemaReady(db *gorm.DB) (bool, error) {
	for table, columns := range articleColumns {
		var names []string
		if err := db.Raw("SELECT name FROM pragma_table_info(?)", table).Scan(&names).Error; err != nil {
			return false, err
		}
		present := map[string]bool{}
		for _, name := range names {
			present[name] = true
		}
		for _, name := range columns {
			if !present[name] {
				return false, nil
			}
		}
	}
	return true, nil
}
func requireArticles(c *fiber.Ctx) (bool, error) {
	ready, err := articleSchemaReady(dbFor(c))
	if err != nil {
		return false, queryFailed(c, err, "Failed to inspect articles")
	}
	if !ready {
		return false, c.Status(503).JSON(fiber.Map{"error": "Articles unavailable for this artifact"})
	}
	return true, nil
}
func articleSummary(article Article) Article {
	article.BodyMarkdown = ""
	article.Metadata = nil
	return article
}
func loadArticleDetail(c *fiber.Ctx, article Article) (ArticleDetail, error) {
	detail := ArticleDetail{Article: article, RelatedEvents: []Event{}, Translations: []ArticleTranslation{}}
	if err := dbFor(c).Table("events e").Select("e.*").Joins("JOIN event_articles ea ON ea.event_id=e.id").Where("ea.article_key=?", article.ArticleKey).Order("e.date ASC, e.id ASC").Scan(&detail.RelatedEvents).Error; err != nil {
		return detail, err
	}
	other := "en"
	db := DB_EN
	if resolveLang(c.Query("lang")) == "en" {
		other = "ru"
		db = DB_RU
	}
	db = db.WithContext(c.UserContext())
	ready, err := articleSchemaReady(db)
	if err != nil {
		return detail, err
	}
	if ready {
		var translated []Article
		if err := db.Select("article_key,slug").Where("article_key=? AND status='published'", article.ArticleKey).Find(&translated).Error; err != nil {
			return detail, err
		}
		for _, a := range translated {
			detail.Translations = append(detail.Translations, ArticleTranslation{Locale: other, Slug: a.Slug})
		}
	}
	return detail, nil
}
func listArticlesHandler(c *fiber.Ctx) error {
	if ok, err := requireArticles(c); !ok {
		return err
	}
	articles := []Article{}
	if err := dbFor(c).Raw("SELECT article_key,slug,title,sources,created_at,updated_at,published_at FROM articles WHERE status='published' ORDER BY slug ASC").Scan(&articles).Error; err != nil {
		return queryFailed(c, err, "Failed to retrieve articles")
	}
	return c.JSON(fiber.Map{"articles": articles})
}
func getArticleHandler(c *fiber.Ctx) error {
	slug := c.Params("slug")
	if !articleSlug.MatchString(slug) {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid article slug"})
	}
	if ok, err := requireArticles(c); !ok {
		return err
	}
	var article Article
	err := dbFor(c).Where("slug=? AND status='published'", slug).First(&article).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c.Status(404).JSON(fiber.Map{"error": "Article not found"})
	}
	if err != nil {
		return queryFailed(c, err, "Failed to retrieve article")
	}
	detail, err := loadArticleDetail(c, article)
	if err != nil {
		return queryFailed(c, err, "Failed to retrieve article relations")
	}
	return c.JSON(fiber.Map{"data": detail})
}
func getEventArticlesHandler(c *fiber.Ctx) error {
	id, err := strconv.ParseUint(c.Params("id"), 10, 32)
	if err != nil || id == 0 {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid Event ID format"})
	}
	var event Event
	err = dbFor(c).First(&event, uint(id)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c.Status(404).JSON(fiber.Map{"error": "Event not found"})
	}
	if err != nil {
		return queryFailed(c, err, "Failed to retrieve event")
	}
	if ok, err := requireArticles(c); !ok {
		return err
	}
	var primary *ArticleDetail
	extra := []Article{}
	var links []struct {
		Article
		IsPrimary bool
	}
	err = dbFor(c).Raw("SELECT articles.*,ea.is_primary FROM articles JOIN event_articles ea ON ea.article_key=articles.article_key WHERE ea.event_id=? AND articles.status='published' ORDER BY ea.sort_order ASC,articles.slug ASC", id).Scan(&links).Error
	if err != nil {
		return queryFailed(c, err, "Failed to retrieve event articles")
	}
	for _, link := range links {
		if link.IsPrimary {
			detail, err := loadArticleDetail(c, link.Article)
			if err != nil {
				return queryFailed(c, err, "Failed to retrieve article relations")
			}
			primary = &detail
		} else {
			extra = append(extra, articleSummary(link.Article))
		}
	}
	return c.JSON(fiber.Map{"primary": primary, "extra": extra})
}
