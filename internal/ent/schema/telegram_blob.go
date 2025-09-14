package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

type TelegramBlob struct {
	ent.Schema
}

// Fields of the TelegramBlob.
func (TelegramBlob) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.New()).
			Default(uuid.New),
		field.Int64("size"),
		field.String("path").Unique(),
		field.String("uri").Unique(),
		field.String("sha256").Comment("hex"),
		field.Bytes("file_reference"),
		field.Int64("file_id"),
		field.Int64("access_hash"),
	}
}
