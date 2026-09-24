package repository

import (
	"encoding/json"
	"reflect"
	"testing"

	"pai-smart-go/internal/model"
)

func TestConversationHistoryRoundTripsTrustedSources(t *testing.T) {
	sources := []model.SourceCitation{{Number: 1, DocumentID: 12, Version: "v1", FileName: "同名.pdf"}, {Number: 2, DocumentID: 37, Version: "v2", FileName: "同名.pdf", SourceSpans: []model.SourceSpan{{BlockID: "b", End: 3}}}}
	turn := model.Conversation{UserID: 7, ConversationID: "fixed", TurnID: "t1", Question: "问题", Answer: "回答 [来源#2]", Sources: sources}
	if err := validateTurn(7, "fixed", turn); err != nil {
		t.Fatal(err)
	}
	messages := messagesFromTurns([]model.Conversation{turn})
	if len(messages[0].Sources) != 0 || !reflect.DeepEqual(messages[1].Sources, sources) {
		t.Fatalf("history sources missing or bound to user message: %+v", messages)
	}
	encoded, _ := json.Marshal(messages)
	var restored []model.ChatMessage
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	turns, err := turnsFromMessages(7, "fixed", restored)
	if err != nil || len(turns) != 1 || !reflect.DeepEqual(turns[0].Sources, sources) {
		t.Fatalf("history/cache reconstruction lost identity: %+v %v", turns, err)
	}
	for _, invalid := range [][]model.SourceCitation{{{Number: 1, FileName: "同名.pdf"}}, {sources[0], sources[0]}, {{Number: 25, DocumentID: 12, Version: "v1"}}} {
		turn.Sources = invalid
		if validateTurn(7, "fixed", turn) == nil {
			t.Fatalf("invalid citation identity accepted: %+v", invalid)
		}
	}
}
