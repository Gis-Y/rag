package storage

import (
	"strconv"
)

// ChunkObjectKey 是分片上传、合并和清理共同使用的对象键。
func ChunkObjectKey(userID, documentID uint, chunkIndex int) string {
	return documentUploadPrefix("chunks", userID, documentID) + strconv.Itoa(chunkIndex)
}

// ChunkAttemptObjectKey isolates concurrent uploads until the SQL upload row
// authorizes publishing one attempt to the canonical chunk key.
func ChunkAttemptObjectKey(userID, documentID uint, chunkIndex int, attempt string) string {
	return documentUploadPrefix("chunks", userID, documentID) + "attempts/" + strconv.Itoa(chunkIndex) + "/" + attempt
}

// DocumentObjectKey 是合并、处理、下载、预览和删除共同使用的对象键。
func DocumentObjectKey(userID, documentID uint, mergeToken string) string {
	return documentUploadPrefix("merged", userID, documentID) + mergeToken
}

func documentUploadPrefix(kind string, userID, documentID uint) string {
	return kind + "/" + strconv.FormatUint(uint64(userID), 10) + "/" + strconv.FormatUint(uint64(documentID), 10) + "/"
}

func DocumentIRObjectKey(userID, documentID uint, version string) string {
	return documentIRPrefix(userID, documentID) + version + "/ir.json"
}

func documentIRPrefix(userID, documentID uint) string {
	return "document-ir/" + strconv.FormatUint(uint64(userID), 10) + "/" + strconv.FormatUint(uint64(documentID), 10) + "/"
}
