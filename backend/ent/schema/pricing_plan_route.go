package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PricingPlanRoute holds the schema definition for the PricingPlanRoute entity.
// 套餐内的内部池层：每组（group_id）在套餐里占据一层出站池，按 priority
// 升序裁决（低值优先）。同一套餐内 (plan_id, group_id) 与 (plan_id, priority)
// 在未删除行上各自唯一（见迁移中的部分唯一索引）。
type PricingPlanRoute struct {
	ent.Schema
}

func (PricingPlanRoute) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "pricing_plan_routes"},
	}
}

func (PricingPlanRoute) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (PricingPlanRoute) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("plan_id"),
		field.Int64("group_id").
			Comment("Internal pool layer: the group whose channels serve this layer."),
		field.Int("priority").
			Default(100).
			Comment("Lower values win; unique among active layers of the same plan."),
		field.Bool("enabled").
			Default(true),
	}
}

func (PricingPlanRoute) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("plan", PricingPlan.Type).
			Unique().
			Required().
			Field("plan_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("group", Group.Type).
			Unique().
			Required().
			Field("group_id").
			Annotations(entsql.OnDelete(entsql.Restrict)),
	}
}

func (PricingPlanRoute) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("plan_id"),
		index.Fields("plan_id", "enabled"),
		index.Fields("group_id"),
		index.Fields("deleted_at"),
		index.Fields("priority"),
	}
}
