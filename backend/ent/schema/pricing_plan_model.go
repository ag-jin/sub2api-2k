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

// PricingPlanModel holds the schema definition for the PricingPlanModel entity.
// 套餐内的「模型 -> 协议」条目：声明某个公开模型在该套餐下走哪种上游
// API 协议（chat_completions / messages / responses）。
type PricingPlanModel struct {
	ent.Schema
}

func (PricingPlanModel) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "pricing_plan_models"},
	}
}

func (PricingPlanModel) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (PricingPlanModel) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("plan_id"),
		field.String("public_model").
			MaxLen(200).
			NotEmpty().
			Comment("Client-facing model identifier."),
		field.String("protocol").
			MaxLen(20).
			Default(domain.PricingPlanProtocolChatCompletions).
			Comment("Upstream API protocol served for this model."),
		field.String("upstream_model").
			MaxLen(200).
			Default("").
			Comment("Provider model identifier; empty means public_model."),
		field.Bool("direct").
			Default(true).
			Comment("Serve the upstream directly through this protocol."),
		field.Bool("allow_compatibility_fallback").
			Default(false).
			Comment("Fall back to a compatibility layer when direct serving fails."),
		field.Int("priority").
			Default(100).
			Comment("Sort hint; lower values win."),
		field.Bool("enabled").
			Default(true),
		field.String("notes").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		// pricing: 该模型协议条目的定价文档（domain.PlanModelPricing，JSONB），
		// 形状与 service.ChannelModelPricing 的定价部分一致；nil 表示未配置。
		field.JSON("pricing", &domain.PlanModelPricing{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("Model pricing document (billing mode + price fields/intervals)."),
	}
}

func (PricingPlanModel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("plan", PricingPlan.Type).
			Unique().
			Required().
			Field("plan_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

func (PricingPlanModel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("plan_id"),
		index.Fields("plan_id", "public_model", "protocol"),
		index.Fields("plan_id", "enabled"),
		index.Fields("deleted_at"),
		index.Fields("priority"),
	}
}
