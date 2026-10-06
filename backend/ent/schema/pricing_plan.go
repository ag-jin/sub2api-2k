package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PricingPlan holds the schema definition for the PricingPlan entity.
// 定价套餐：聚合「模型 -> 协议」条目与按优先级排序的路由，供管理端维护、
// 认证/产品浏览端读取。API key 可选用一个套餐（见 api_keys.pricing_plan_id）。
type PricingPlan struct {
	ent.Schema
}

func (PricingPlan) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "pricing_plans"},
	}
}

func (PricingPlan) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (PricingPlan) Fields() []ent.Field {
	return []ent.Field{
		// 稳定标识键，软删除后允许重用（唯一约束通过迁移中的部分索引实现）
		field.String("name").
			MaxLen(100).
			NotEmpty().
			Comment("Stable plan key, unique among non-deleted rows."),
		field.String("title").
			MaxLen(100).
			Default("").
			Comment("Public-facing display name."),
		field.String("description").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Comment("Public-facing plan description."),
		field.String("status").
			MaxLen(20).
			Default(domain.StatusActive),
		field.Bool("is_public").
			Default(false).
			Comment("Exposed as a purchasable product in public listings."),
		field.Int("sort_order").
			Default(0).
			Comment("Ascending display order for product listings."),
	}
}

func (PricingPlan) Edges() []ent.Edge {
	return []ent.Edge{
		// 逆边引用子表各自的 plan 边：外键列只存在于子表（plan_id），
		// 避免 Ent 为独立 To 边在子表生成多余的 FK 列。
		edge.From("models", PricingPlanModel.Type).
			Ref("plan"),
		edge.From("routes", PricingPlanRoute.Type).
			Ref("plan"),
		edge.To("api_keys", APIKey.Type),
	}
}

func (PricingPlan) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name"),
		index.Fields("status"),
		index.Fields("is_public", "status"),
		index.Fields("sort_order"),
		index.Fields("deleted_at"),
	}
}
