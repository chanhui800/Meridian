package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
	_ "modernc.org/sqlite"
)

const (
	backupMagic               = "MRDBKP01" // v1 reader compatibility
	backupMagicV2             = "MRDBKP02"
	backupFormatVersion       = 2
	backupLegacyFormatVersion = 1
	backupSaltBytes           = 16
	backupV2ChunkBytes        = 4 << 20
	backupMaxUploadBytes      = 256 << 20
	backupMaxExpandedBytes    = 512 << 20
	// Keep the entry-count ceiling comfortably above the current per-node TLS
	// layout (two files per node) while retaining the independent expanded-size
	// and per-entry limits below.
	backupMaxFiles           = 4096
	backupMinPasswordBytes   = 12
	backupMaxPasswordBytes   = 128
	backupPendingSuffix      = ".restore-pending"
	backupAppliedSuffix      = ".restore-applied"
	backupRollbackSuffix     = ".restore-rollback"
	backupDatabaseEntry      = "meridian.db"
	backupManifestEntry      = "manifest.json"
	backupTLSCertificate     = "tls/fullchain.pem"
	backupTLSPrivateKey      = "tls/privkey.pem"
	backupTLSEnabled         = "tls/enabled"
	backupACMEAccount        = "tls/acme-account.pem"
	backupACMEAccountStaging = "tls/acme-account-staging.pem"
	backupTLSEdgeNodesPrefix = "tls/edge-nodes/"
)

var backupAllowedEntries = map[string]int64{
	backupManifestEntry:      1 << 20,
	backupDatabaseEntry:      backupMaxExpandedBytes,
	backupTLSCertificate:     4 << 20,
	backupTLSPrivateKey:      1 << 20,
	backupTLSEnabled:         64,
	backupACMEAccount:        1 << 20,
	backupACMEAccountStaging: 1 << 20,
}

func backupEntryLimit(name string) (int64, bool) {
	if limit, ok := backupAllowedEntries[name]; ok {
		return limit, true
	}
	if !strings.HasPrefix(name, backupTLSEdgeNodesPrefix) {
		return 0, false
	}
	parts := strings.Split(strings.TrimPrefix(name, backupTLSEdgeNodesPrefix), "/")
	if len(parts) != 2 || parts[0] == "" || (parts[1] != "fullchain.pem" && parts[1] != "privkey.pem") {
		return 0, false
	}
	for _, r := range parts[0] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return 0, false
		}
	}
	return 4 << 20, true
}

func backupTLSEntries(manifestFiles []string) []string {
	values := make([]string, 0, len(manifestFiles))
	for _, name := range manifestFiles {
		if strings.HasPrefix(name, "tls/") {
			if _, ok := backupEntryLimit(name); ok {
				values = append(values, name)
			}
		}
	}
	return values
}

type backupManifest struct {
	Format                string   `json:"format"`
	FormatVersion         int      `json:"format_version"`
	AppVersion            string   `json:"app_version"`
	DatabaseSchemaVersion int      `json:"database_schema_version,omitempty"`
	CreatedAt             string   `json:"created_at"`
	Files                 []string `json:"files"`
	IncludeTLS            *bool    `json:"include_tls,omitempty"`
	JWTSecret             string   `json:"jwt_secret"` // #nosec G117 -- encrypted backup manifest field; it is never logged or persisted outside the encrypted archive.
	UpstreamHeaderKey     string   `json:"upstream_header_key"`
}

type restoreAppliedState struct {
	RollbackDir string
}

type restoreMarker struct {
	Files      []string `json:"files"`
	IncludeTLS *bool    `json:"include_tls,omitempty"`
}

type backupPanelSettings struct {
	PanelDomain         string
	RouteDomain         string
	ListenPort          int
	TLSEnabled          int
	Configured          int
	ACMEEmail           string
	ACMEDNSProvider     string
	ACMETokenCiphertext string
	ACMEStaging         int
}

func boolPointer(value bool) *bool { return &value }

func manifestIncludesTLS(manifest backupManifest) bool {
	// Backups created before include_tls was introduced always included TLS.
	return manifest.IncludeTLS == nil || *manifest.IncludeTLS
}

func markerIncludesTLS(marker restoreMarker) bool {
	// Pending restores created before include_tls was introduced always replaced TLS.
	return marker.IncludeTLS == nil || *marker.IncludeTLS
}

func restoreDirectoryIncludesTLS(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "restore.json")) // #nosec G304 G703 -- dir is an internal rollback directory created by Meridian.
	if err != nil {
		// Restores created before include_tls was introduced always replaced TLS.
		return true
	}
	var marker restoreMarker
	if json.Unmarshal(data, &marker) != nil {
		return true
	}
	return markerIncludesTLS(marker)
}

func restoreDirectoryTLSEntries(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, "restore.json")) // #nosec G304 G703 -- dir is an internal rollback directory created by Meridian.
	if err != nil {
		values := make([]string, 0, len(backupAllowedEntries))
		for name := range backupAllowedEntries {
			if strings.HasPrefix(name, "tls/") {
				values = append(values, name)
			}
		}
		return values
	}
	var marker restoreMarker
	if json.Unmarshal(data, &marker) != nil {
		return nil
	}
	return backupTLSEntries(marker.Files)
}

func validateBackupPassword(password string) error {
	if len(password) < backupMinPasswordBytes || len(password) > backupMaxPasswordBytes {
		return fmt.Errorf("备份密码必须为 %d-%d 个字节", backupMinPasswordBytes, backupMaxPasswordBytes)
	}
	return nil
}

func backupKey(password string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(password), salt, 32768, 8, 1, 32)
}

func sealBackup(plain []byte, password string) ([]byte, error) {
	if err := validateBackupPassword(password); err != nil {
		return nil, err
	}
	salt := make([]byte, backupSaltBytes)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("生成备份盐值: %w", err)
	}
	key, err := backupKey(password, salt)
	if err != nil {
		return nil, fmt.Errorf("派生备份密钥: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("生成备份随机数: %w", err)
	}
	header := append([]byte(backupMagic), salt...)
	if len(nonce) > 255 {
		return nil, errors.New("backup nonce is too large")
	}
	header = append(header, byte(len(nonce))) // #nosec G115 -- nonce length is bounded by the AES-GCM implementation and checked above.
	header = append(header, nonce...)
	sealed := gcm.Seal(nil, nonce, plain, header)
	return append(header, sealed...), nil
}

// sealBackupV2 encrypts independent, ordered chunks. Each record carries its
// index and plaintext length in authenticated data, so reordering, omission,
// duplication, truncation, and length forgery all fail closed in openBackup.
func sealBackupV2(plain []byte, password string) ([]byte, error) {
	var out bytes.Buffer
	if _, err := sealBackupV2Reader(bytes.NewReader(plain), password, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// sealBackupV2Reader is the streaming v2 writer.  The compatibility wrapper
// above remains useful for small unit fixtures, while production backup
// creation feeds the ZIP from a private temporary file so the complete ZIP
// and encrypted payload are never resident in memory at the same time.
func sealBackupV2Reader(reader io.Reader, password string, writer io.Writer) (int64, error) {
	if err := validateBackupPassword(password); err != nil {
		return 0, err
	}
	salt := make([]byte, backupSaltBytes)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return 0, fmt.Errorf("生成备份盐值: %w", err)
	}
	key, err := backupKey(password, salt)
	if err != nil {
		return 0, fmt.Errorf("派生备份密钥: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return 0, err
	}
	written := int64(0)
	write := func(data []byte) error {
		n, err := writer.Write(data)
		written += int64(n)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		return nil
	}
	if err := write([]byte(backupMagicV2)); err != nil {
		return written, err
	}
	if err := write(salt); err != nil {
		return written, err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], backupV2ChunkBytes)
	if err := write(size[:]); err != nil {
		return written, err
	}
	writeRecord := func(index uint32, part []byte) error {
		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return fmt.Errorf("生成备份随机数: %w", err)
		}
		sealed := gcm.Seal(nil, nonce, part, backupV2AAD(index, uint32(len(part)))) // #nosec G115 -- part is bounded by backupV2ChunkBytes.
		var record [12]byte
		binary.BigEndian.PutUint32(record[0:4], index)
		binary.BigEndian.PutUint32(record[4:8], uint32(len(part)))    // #nosec G115 -- part is bounded by backupV2ChunkBytes.
		binary.BigEndian.PutUint32(record[8:12], uint32(len(sealed))) // #nosec G115 -- sealed is a bounded chunk plus the GCM tag.
		if err := write(record[:]); err != nil {
			return err
		}
		if err := write(nonce); err != nil {
			return err
		}
		return write(sealed)
	}
	buffer := make([]byte, backupV2ChunkBytes)
	var index uint32
	for {
		n, readErr := io.ReadFull(reader, buffer)
		if n > 0 {
			if err := writeRecord(index, buffer[:n]); err != nil {
				return written, err
			}
			index++
		}
		if readErr == nil {
			continue
		}
		if readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return written, readErr
		}
		break
	}
	// A zero-length authenticated record is an explicit end-of-plaintext
	// marker. It also makes truncation at an exact chunk boundary detectable.
	if err := writeRecord(index, nil); err != nil {
		return written, err
	}
	// An explicit authenticated stream terminator lets the reader distinguish a
	// complete exact-size stream from a truncated final chunk.
	if err := write([]byte("END!")); err != nil {
		return written, err
	}
	return written, nil
}

func backupV2AAD(index, plainLen uint32) []byte {
	var aad [8 + 4 + 4 + 4]byte
	copy(aad[:], backupMagicV2)
	binary.BigEndian.PutUint32(aad[8:12], backupFormatVersion)
	binary.BigEndian.PutUint32(aad[12:16], index)
	binary.BigEndian.PutUint32(aad[16:20], plainLen)
	return aad[:]
}

func openBackup(payload []byte, password string) ([]byte, error) {
	if err := validateBackupPassword(password); err != nil {
		return nil, err
	}
	if len(payload) >= len(backupMagicV2) && string(payload[:len(backupMagicV2)]) == backupMagicV2 {
		return openBackupV2(payload, password)
	}
	minimum := len(backupMagic) + backupSaltBytes + 1
	if len(payload) < minimum || string(payload[:len(backupMagic)]) != backupMagic {
		return nil, errors.New("不是有效的 Meridian 备份文件")
	}
	saltStart := len(backupMagic)
	salt := payload[saltStart : saltStart+backupSaltBytes]
	nonceLen := int(payload[saltStart+backupSaltBytes])
	headerLen := minimum + nonceLen
	if nonceLen < 1 || headerLen >= len(payload) {
		return nil, errors.New("备份文件头损坏")
	}
	key, err := backupKey(password, salt)
	if err != nil {
		return nil, fmt.Errorf("派生备份密钥: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if nonceLen != gcm.NonceSize() || len(payload)-headerLen < gcm.Overhead() {
		return nil, errors.New("备份文件头损坏")
	}
	plain, err := gcm.Open(nil, payload[minimum:headerLen], payload[headerLen:], payload[:headerLen])
	if err != nil {
		return nil, errors.New("备份密码错误或文件已被篡改")
	}
	return plain, nil
}

func openBackupV2(payload []byte, password string) ([]byte, error) {
	var plain bytes.Buffer
	if _, err := openBackupV2Reader(bytes.NewReader(payload), password, &plain); err != nil {
		return nil, err
	}
	return plain.Bytes(), nil
}

// openBackupV2Reader authenticates and decrypts one chunk at a time.  The
// writer is deliberately supplied by the caller so restore handlers can keep
// the plaintext ZIP on disk instead of retaining the encrypted upload and the
// decrypted archive simultaneously.
func openBackupV2Reader(reader io.Reader, password string, writer io.Writer) (int64, error) {
	if err := validateBackupPassword(password); err != nil {
		return 0, err
	}
	minimum := len(backupMagicV2) + backupSaltBytes + 4
	header := make([]byte, minimum)
	if _, err := io.ReadFull(reader, header); err != nil || string(header[:len(backupMagicV2)]) != backupMagicV2 {
		return 0, errors.New("不是有效的 Meridian v2 备份文件")
	}
	saltStart := len(backupMagicV2)
	salt := header[saltStart : saltStart+backupSaltBytes]
	chunkSize := binary.BigEndian.Uint32(header[saltStart+backupSaltBytes : minimum])
	if chunkSize == 0 || chunkSize > backupV2ChunkBytes {
		return 0, errors.New("备份分块大小无效")
	}
	key, err := backupKey(password, salt)
	if err != nil {
		return 0, fmt.Errorf("派生备份密钥: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return 0, err
	}
	buffer := bufio.NewReaderSize(reader, 64<<10)
	var expected uint32
	shortChunkSeen := false
	endChunkSeen := false
	var expanded int64
	for {
		marker := make([]byte, 4)
		if _, err := io.ReadFull(buffer, marker); err != nil {
			return 0, errors.New("备份分块缺失")
		}
		if string(marker) == "END!" {
			var trailing [1]byte
			if n, err := buffer.Read(trailing[:]); n != 0 || err != io.EOF {
				return 0, errors.New("备份包含尾随数据")
			}
			if !endChunkSeen {
				return 0, errors.New("备份分块结束标记缺失")
			}
			return expanded, nil
		}
		meta := make([]byte, 8)
		if _, err := io.ReadFull(buffer, meta); err != nil {
			return 0, errors.New("备份分块缺失")
		}
		index := binary.BigEndian.Uint32(marker)
		plainLen := binary.BigEndian.Uint32(meta[:4])
		cipherLen := binary.BigEndian.Uint32(meta[4:])
		if index != expected || endChunkSeen || (shortChunkSeen && plainLen != 0) || plainLen > chunkSize || cipherLen != plainLen+uint32(gcm.Overhead()) || cipherLen > backupMaxExpandedBytes || int64(plainLen)+expanded > backupMaxExpandedBytes { // #nosec G115 -- GCM overhead is a fixed small constant.
			return 0, errors.New("备份分块顺序或长度无效")
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(buffer, nonce); err != nil {
			return 0, errors.New("备份分块缺失")
		}
		sealed := make([]byte, cipherLen)
		if _, err := io.ReadFull(buffer, sealed); err != nil {
			return 0, errors.New("备份分块缺失")
		}
		part, err := gcm.Open(nil, nonce, sealed, backupV2AAD(index, plainLen))
		if err != nil {
			return 0, errors.New("备份密码错误或文件已被篡改")
		}
		if uint32(len(part)) != plainLen { // #nosec G115 -- decrypted part is bounded by the authenticated chunk length.
			return 0, errors.New("备份分块长度无效")
		}
		if plainLen == 0 {
			endChunkSeen = true
		} else if plainLen < chunkSize {
			shortChunkSeen = true
		}
		if n, err := writer.Write(part); err != nil {
			return expanded, err
		} else if n != len(part) {
			return expanded, io.ErrShortWrite
		}
		expanded += int64(len(part))
		expected++
	}
}

func quoteSQLiteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (a *App) databaseSnapshot() (string, func(), error) {
	if a == nil || a.db == nil || a.dbPath == "" || a.dbPath == ":memory:" || strings.HasPrefix(a.dbPath, "file:") {
		return "", func() {}, errors.New("当前数据库模式不支持备份")
	}
	// VACUUM INTO must not be the first operation that discovers a nearly full
	// filesystem. Account for the live database and SQLite sidecars before any
	// snapshot directory is created. The later check covers the actual snapshot
	// size, which may be larger after vacuuming.
	var databaseBytes int64
	for _, candidate := range []string{a.dbPath, a.dbPath + "-wal", a.dbPath + "-shm"} {
		info, statErr := os.Stat(candidate)
		if statErr == nil {
			if !info.Mode().IsRegular() {
				return "", func() {}, errors.New("数据库文件必须是普通文件")
			}
			databaseBytes += info.Size()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", func() {}, fmt.Errorf("检查数据库空间: %w", statErr)
		}
	}
	if databaseBytes <= 0 {
		databaseBytes = 1 << 20
	}
	archiveEstimate := databaseBytes
	if archiveEstimate > backupMaxUploadBytes {
		archiveEstimate = backupMaxUploadBytes
	}
	if err := ensureDiskSpace(filepath.Dir(a.dbPath), databaseBytes*2+archiveEstimate+(64<<20)); err != nil {
		return "", func() {}, err
	}
	if a.pm != nil {
		a.pm.FlushTraffic()
	}
	if err := a.db.flushDynamicObservations(); err != nil {
		return "", func() {}, fmt.Errorf("刷新日志队列: %w", err)
	}
	dir, err := os.MkdirTemp(filepath.Dir(a.dbPath), ".meridian-backup-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	snapshot := filepath.Join(dir, backupDatabaseEntry)
	if _, err := a.db.db.Exec("VACUUM INTO " + quoteSQLiteString(snapshot)); err != nil { // #nosec G202 -- snapshot is created in a private temporary directory and quoteSQLiteString escapes the complete literal.
		cleanup()
		return "", func() {}, fmt.Errorf("创建 SQLite 一致性快照: %w", err)
	}
	info, err := os.Stat(snapshot)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("检查 SQLite 快照大小: %w", err)
	}
	if info.Size() > backupMaxExpandedBytes {
		cleanup()
		return "", func() {}, fmt.Errorf("数据库大小 %d MiB 超过当前备份格式可恢复上限 %d MiB", info.Size()>>20, backupMaxExpandedBytes>>20)
	}
	if err := hardenDatabaseFilePermissions(snapshot); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return snapshot, cleanup, nil
}

func addZipFile(writer *zip.Writer, name, path string) (bool, int64, error) {
	limit, ok := backupEntryLimit(name)
	if !ok || name == backupManifestEntry {
		return false, 0, fmt.Errorf("不允许的备份条目: %s", name)
	}
	file, err := os.Open(path) // #nosec G304 -- path is selected from internal database/TLS files before entering the archive.
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, 0, err
	}
	if !info.Mode().IsRegular() {
		return false, 0, errors.New("备份条目必须是普通文件")
	}
	if info.Size() > limit {
		return false, 0, fmt.Errorf("备份条目 %s 超过大小限制", name)
	}
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o600)
	header.Modified = time.Unix(0, 0).UTC()
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return false, 0, err
	}
	written, err := io.Copy(entry, io.LimitReader(file, limit+1))
	if err != nil {
		return false, written, err
	}
	if written > limit {
		return false, written, fmt.Errorf("备份条目 %s 超过大小限制", name)
	}
	return true, written, nil
}

func addBackupEntry(writer *zip.Writer, files *[]string, expandedSize *int64, name, path string) error {
	if files == nil || expandedSize == nil {
		return errors.New("备份条目状态为空")
	}
	if _, ok := backupEntryLimit(name); !ok || name == backupManifestEntry {
		return fmt.Errorf("不允许的备份条目: %s", name)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) { // #nosec G304 -- path is selected from internal database/TLS files before entering the archive.
		return nil
	} else if err != nil {
		return err
	}
	// The manifest is added after all data entries. Reserve one slot for it
	// while admitting the candidate entry so exported backups are always
	// accepted by parseBackupArchive.
	if len(*files)+2 > backupMaxFiles {
		return errors.New("备份文件数量超过可恢复上限")
	}
	added, size, err := addZipFile(writer, name, path)
	if err != nil {
		return err
	}
	if !added {
		return nil
	}
	if *expandedSize > backupMaxExpandedBytes-size {
		return errors.New("备份解压后总大小超出限制")
	}
	*expandedSize += size
	*files = append(*files, name)
	return nil
}

type builtBackupFile struct {
	Path    string
	Size    int64
	Cleanup func()
}

func (a *App) buildBackupToFile(password string, includeTLS bool) (builtBackupFile, error) {
	if err := validateBackupPassword(password); err != nil {
		return builtBackupFile{}, err
	}
	if includeTLS && a != nil {
		if err := validateTLSPathConfiguration(a.dbPath); err != nil {
			return builtBackupFile{}, fmt.Errorf("invalid TLS path configuration: %w", err)
		}
	}
	snapshot, cleanup, err := a.databaseSnapshot()
	if err != nil {
		return builtBackupFile{}, err
	}
	retainSnapshot := false
	defer func() {
		if !retainSnapshot {
			cleanup()
		}
	}()
	if info, statErr := os.Stat(snapshot); statErr == nil {
		// Snapshot, ZIP, encrypted payload, and a small rollback margin coexist
		// for the duration of export. Refuse early when the filesystem cannot hold
		// those bounded temporary copies.
		required := info.Size()*3 + 64<<20
		if err := ensureDiskSpace(filepath.Dir(snapshot), required); err != nil {
			return builtBackupFile{}, err
		}
	} else {
		return builtBackupFile{}, statErr
	}
	archivePath := filepath.Join(filepath.Dir(snapshot), "archive.zip")
	archiveFile, err := os.OpenFile(archivePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- archivePath is inside the private snapshot directory.
	if err != nil {
		return builtBackupFile{}, err
	}
	defer archiveFile.Close()
	zipWriter := zip.NewWriter(archiveFile)
	files := make([]string, 0, 8)
	expandedSize := int64(0)
	if err := addBackupEntry(zipWriter, &files, &expandedSize, backupDatabaseEntry, snapshot); err != nil {
		return builtBackupFile{}, err
	}
	if includeTLS {
		certFile, keyFile := panelTLSBackupPaths(a.dbPath)
		tlsDir := tlsStateDir(a.dbPath)
		if certFile != "" && keyFile != "" {
			_, certErr := os.Stat(certFile)
			_, keyErr := os.Stat(keyFile)
			certExists := certErr == nil
			keyExists := keyErr == nil
			if certExists != keyExists {
				return builtBackupFile{}, errors.New("面板 TLS 证书和私钥不成对，无法创建包含 TLS 的备份")
			}
			if certErr != nil && !errors.Is(certErr, os.ErrNotExist) {
				return builtBackupFile{}, fmt.Errorf("检查面板 TLS 证书: %w", certErr)
			}
			if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
				return builtBackupFile{}, fmt.Errorf("检查面板 TLS 私钥: %w", keyErr)
			}
		}
		tlsCandidates := []struct{ name, path string }{
			{backupTLSCertificate, certFile},
			{backupTLSPrivateKey, keyFile},
		}
		if tlsDir != "" {
			tlsCandidates = append(tlsCandidates,
				struct{ name, path string }{backupTLSEnabled, filepath.Join(tlsDir, "enabled")},
				struct{ name, path string }{backupACMEAccount, filepath.Join(tlsDir, "acme-account.pem")},
				struct{ name, path string }{backupACMEAccountStaging, filepath.Join(tlsDir, "acme-account-staging.pem")},
			)
		}
		for _, candidate := range tlsCandidates {
			if candidate.path == "" {
				continue
			}
			if err := addBackupEntry(zipWriter, &files, &expandedSize, candidate.name, candidate.path); err != nil {
				return builtBackupFile{}, fmt.Errorf("读取 %s: %w", candidate.name, err)
			}
		}
		// Per-node Edge certificates live behind an atomic current pointer.
		// Back up the resolved pair under stable archive names; restore rebuilds
		// each node's current directory from these files.
		if edgeRoot := edgeNodeTLSRoot(a.dbPath); edgeRoot != "" {
			entries, readErr := os.ReadDir(edgeRoot)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return builtBackupFile{}, fmt.Errorf("读取 Edge 节点证书目录: %w", readErr)
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				guid := entry.Name()
				for _, filename := range []string{"fullchain.pem", "privkey.pem"} {
					name := backupTLSEdgeNodesPrefix + guid + "/" + filename
					path := filepath.Join(edgeRoot, guid, "current", filename)
					if filename == "fullchain.pem" {
						keyPath := filepath.Join(edgeRoot, guid, "current", "privkey.pem")
						_, certErr := os.Stat(path)
						_, keyErr := os.Stat(keyPath)
						certExists := certErr == nil
						keyExists := keyErr == nil
						if certExists != keyExists {
							return builtBackupFile{}, fmt.Errorf("Edge 节点 %s 的证书和私钥不成对", guid)
						}
						if !certExists {
							if certErr != nil && !errors.Is(certErr, os.ErrNotExist) || keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
								return builtBackupFile{}, fmt.Errorf("检查 Edge 节点 %s 证书: %v / %v", guid, certErr, keyErr)
							}
							break
						}
					}
					if addErr := addBackupEntry(zipWriter, &files, &expandedSize, name, path); addErr != nil {
						return builtBackupFile{}, fmt.Errorf("读取 %s: %w", name, addErr)
					}
				}
			}
		}
	}
	if len(files)+1 > backupMaxFiles {
		return builtBackupFile{}, errors.New("备份文件数量超过可恢复上限")
	}
	manifest := backupManifest{
		Format:                "meridian-backup",
		FormatVersion:         backupFormatVersion,
		AppVersion:            appVersion,
		DatabaseSchemaVersion: databaseSchemaVersion,
		CreatedAt:             time.Now().UTC().Format(time.RFC3339),
		Files:                 files,
		IncludeTLS:            boolPointer(includeTLS),
		JWTSecret:             base64.RawStdEncoding.EncodeToString(jwtSecret),
		UpstreamHeaderKey:     base64.RawStdEncoding.EncodeToString(a.pm.upstreamHeaderKey),
	}
	manifestData, err := json.Marshal(manifest) // #nosec G117 -- the manifest is immediately encrypted before it leaves the process.
	if err != nil {
		return builtBackupFile{}, err
	}
	header := &zip.FileHeader{Name: backupManifestEntry, Method: zip.Deflate}
	header.SetMode(0o600)
	header.Modified = time.Unix(0, 0).UTC()
	entry, err := zipWriter.CreateHeader(header)
	if err != nil {
		return builtBackupFile{}, err
	}
	if _, err := entry.Write(manifestData); err != nil {
		return builtBackupFile{}, err
	}
	if expandedSize+int64(len(manifestData)) > backupMaxExpandedBytes {
		return builtBackupFile{}, errors.New("备份解压后总大小超出限制")
	}
	if err := zipWriter.Close(); err != nil {
		return builtBackupFile{}, err
	}
	if err := archiveFile.Sync(); err != nil {
		return builtBackupFile{}, err
	}
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		return builtBackupFile{}, err
	}
	payloadPath := filepath.Join(filepath.Dir(snapshot), "backup.mrbak")
	payloadFile, err := os.OpenFile(payloadPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- payloadPath is inside the private snapshot directory.
	if err != nil {
		return builtBackupFile{}, err
	}
	defer payloadFile.Close()
	payloadSize, err := sealBackupV2Reader(archiveFile, password, payloadFile)
	if err != nil {
		return builtBackupFile{}, err
	}
	if payloadSize > backupMaxUploadBytes {
		return builtBackupFile{}, fmt.Errorf("生成的备份为 %d MiB，超过当前恢复接口支持的 %d MiB", payloadSize>>20, backupMaxUploadBytes>>20)
	}
	if err := payloadFile.Sync(); err != nil {
		return builtBackupFile{}, err
	}
	if err := syncDirectory(filepath.Dir(payloadPath)); err != nil {
		return builtBackupFile{}, err
	}
	retainSnapshot = true
	return builtBackupFile{Path: payloadPath, Size: payloadSize, Cleanup: cleanup}, nil
}

// buildBackup keeps the byte-slice API for unit tests and legacy callers. The
// production HTTP path uses buildBackupToFile so it never materializes the
// final encrypted backup in memory.
func (a *App) buildBackup(password string, includeTLS bool) ([]byte, error) {
	artifact, err := a.buildBackupToFile(password, includeTLS)
	if err != nil {
		return nil, err
	}
	defer artifact.Cleanup()
	return os.ReadFile(artifact.Path) // #nosec G304 -- path is a private temporary backup artifact.
}

func readZipEntry(file *zip.File, maxBytes int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(maxBytes) { // #nosec G115 -- maxBytes is a positive, fixed per-entry limit selected by Meridian.
		return nil, fmt.Errorf("%s 解压后过大", file.Name)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s 解压后过大", file.Name)
	}
	return data, nil
}

func parseBackupArchive(plain []byte) (backupManifest, map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		return backupManifest{}, nil, errors.New("备份压缩包损坏")
	}
	return parseBackupZipReader(reader)
}

func parseBackupArchiveFile(path string) (backupManifest, map[string][]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- path is an internal restore staging file.
	if err != nil {
		return backupManifest{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return backupManifest{}, nil, err
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return backupManifest{}, nil, errors.New("备份压缩包损坏")
	}
	return parseBackupZipReader(reader)
}

// backupArchiveInspection performs all central-directory checks before any
// archive entry is extracted. This gives restore a disk-space decision point
// while the target filesystem is still untouched.
type backupArchiveInspection struct {
	Manifest      backupManifest
	Entries       []*zip.File
	EntryCount    int
	ExpandedBytes int64
	DatabaseBytes int64
	TLSBytes      int64
}

func inspectBackupArchiveFile(path string) (backupArchiveInspection, error) {
	var result backupArchiveInspection
	file, err := os.Open(path) // #nosec G304 -- path is an internal restore staging file.
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return result, errors.New("备份压缩包损坏")
	}
	if len(reader.File) < 2 || len(reader.File) > backupMaxFiles {
		return result, errors.New("备份文件数量无效")
	}
	seen := make(map[string]struct{}, len(reader.File))
	result.Entries = append([]*zip.File(nil), reader.File...)
	var manifestFile *zip.File
	for _, archiveEntry := range reader.File {
		limit, allowed := backupEntryLimit(archiveEntry.Name)
		if !allowed || filepath.ToSlash(filepath.Clean(archiveEntry.Name)) != archiveEntry.Name || strings.HasPrefix(archiveEntry.Name, "/") {
			return result, fmt.Errorf("备份包含不允许的文件: %s", archiveEntry.Name)
		}
		if _, duplicate := seen[archiveEntry.Name]; duplicate {
			return result, fmt.Errorf("备份包含重复文件: %s", archiveEntry.Name)
		}
		seen[archiveEntry.Name] = struct{}{}
		if archiveEntry.UncompressedSize64 > uint64(limit) || archiveEntry.UncompressedSize64 > uint64(backupMaxExpandedBytes) { // #nosec G115 -- fixed positive limits.
			return result, fmt.Errorf("%s 解压后过大", archiveEntry.Name)
		}
		if archiveEntry.CompressedSize64 > uint64(backupMaxUploadBytes) { // #nosec G115 -- fixed positive limits.
			return result, fmt.Errorf("%s 压缩数据过大", archiveEntry.Name)
		}
		if archiveEntry.UncompressedSize64 > uint64(backupMaxExpandedBytes)-uint64(result.ExpandedBytes) { // #nosec G115 -- fixed positive limits.
			return result, errors.New("备份解压后总大小超出限制")
		}
		result.ExpandedBytes += int64(archiveEntry.UncompressedSize64)
		if archiveEntry.Name == backupDatabaseEntry {
			result.DatabaseBytes = int64(archiveEntry.UncompressedSize64)
		}
		if strings.HasPrefix(archiveEntry.Name, "tls/") {
			result.TLSBytes += int64(archiveEntry.UncompressedSize64)
		}
		if archiveEntry.Name == backupManifestEntry {
			manifestFile = archiveEntry
		}
	}
	if manifestFile == nil {
		return result, errors.New("备份缺少清单")
	}
	manifestData, err := readZipEntry(manifestFile, 1<<20)
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result.Manifest); err != nil {
		return result, errors.New("备份清单无效")
	}
	if result.Manifest.Format != "meridian-backup" || (result.Manifest.FormatVersion != backupFormatVersion && result.Manifest.FormatVersion != backupLegacyFormatVersion) {
		return result, errors.New("不支持的备份格式版本")
	}
	if result.Manifest.DatabaseSchemaVersion > databaseSchemaVersion {
		return result, errors.New("该备份由更高数据库版本的 Meridian 创建，请先升级 Meridian 后再恢复")
	}
	if result.DatabaseBytes <= 0 {
		return result, errors.New("备份缺少数据库")
	}
	if !manifestIncludesTLS(result.Manifest) {
		for name := range seen {
			if strings.HasPrefix(name, "tls/") {
				return result, errors.New("备份清单声明不包含 TLS，但压缩包中存在 TLS 文件")
			}
		}
	}
	declared := make(map[string]struct{}, len(result.Manifest.Files)+1)
	declared[backupManifestEntry] = struct{}{}
	for _, name := range result.Manifest.Files {
		if _, allowed := backupEntryLimit(name); !allowed || name == backupManifestEntry {
			return result, errors.New("备份清单文件列表无效")
		}
		if _, duplicate := declared[name]; duplicate {
			return result, errors.New("备份清单文件列表无效")
		}
		declared[name] = struct{}{}
	}
	if len(declared) != len(seen) {
		return result, errors.New("备份清单与文件内容不一致")
	}
	for name := range seen {
		if _, ok := declared[name]; !ok {
			return result, errors.New("备份清单与文件内容不一致")
		}
	}
	result.EntryCount = len(reader.File)
	return result, nil
}

// parseBackupArchiveFileToPaths validates a decrypted archive while keeping
// each entry on disk. The restore path uses this variant so a legal large
// backup never becomes an in-memory map[string][]byte.
func parseBackupArchiveFileToPaths(path, entriesDir string) (backupManifest, map[string]string, error) {
	var manifest backupManifest
	if _, err := inspectBackupArchiveFile(path); err != nil {
		return manifest, nil, err
	}
	file, err := os.Open(path) // #nosec G304 -- path is an internal restore staging file.
	if err != nil {
		return manifest, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return manifest, nil, err
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return manifest, nil, errors.New("备份压缩包损坏")
	}
	if len(reader.File) < 2 || len(reader.File) > backupMaxFiles {
		return manifest, nil, errors.New("备份文件数量无效")
	}
	if err := os.MkdirAll(entriesDir, 0o700); err != nil {
		return manifest, nil, err
	}
	entries := make(map[string]string, len(reader.File))
	var expanded int64
	for index, archiveEntry := range reader.File {
		limit, allowed := backupEntryLimit(archiveEntry.Name)
		if !allowed || filepath.ToSlash(filepath.Clean(archiveEntry.Name)) != archiveEntry.Name || strings.HasPrefix(archiveEntry.Name, "/") {
			return manifest, nil, fmt.Errorf("备份包含不允许的文件: %s", archiveEntry.Name)
		}
		if _, duplicate := entries[archiveEntry.Name]; duplicate {
			return manifest, nil, fmt.Errorf("备份包含重复文件: %s", archiveEntry.Name)
		}
		if archiveEntry.UncompressedSize64 > uint64(limit) || archiveEntry.UncompressedSize64 > uint64(backupMaxExpandedBytes) { // #nosec G115 -- limits are fixed positive constants returned by backupEntryLimit.
			return manifest, nil, fmt.Errorf("%s 解压后过大", archiveEntry.Name)
		}
		if archiveEntry.UncompressedSize64 > uint64(backupMaxExpandedBytes)-uint64(expanded) {
			return manifest, nil, errors.New("备份解压后总大小超出限制")
		}
		expanded += int64(archiveEntry.UncompressedSize64)
		entryPath := filepath.Join(entriesDir, fmt.Sprintf("%08d.entry", index))
		if err := copyZipEntryToFile(archiveEntry, limit, entryPath); err != nil {
			return manifest, nil, err
		}
		actualInfo, statErr := os.Stat(entryPath)
		if statErr != nil {
			return manifest, nil, statErr
		}
		expandedBefore := expanded - int64(archiveEntry.UncompressedSize64)
		if actualInfo.Size() > limit || actualInfo.Size() > backupMaxExpandedBytes-expandedBefore {
			return manifest, nil, errors.New("备份解压后总大小超出限制")
		}
		expanded = expandedBefore + actualInfo.Size()
		entries[archiveEntry.Name] = entryPath
	}
	manifestPath, ok := entries[backupManifestEntry]
	if !ok {
		return manifest, nil, errors.New("备份缺少清单")
	}
	manifestData, err := os.ReadFile(manifestPath) // #nosec G304 -- path is a private parser staging entry.
	if err != nil {
		return manifest, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, nil, errors.New("备份清单无效")
	}
	if manifest.Format != "meridian-backup" || (manifest.FormatVersion != backupFormatVersion && manifest.FormatVersion != backupLegacyFormatVersion) {
		return manifest, nil, errors.New("不支持的备份格式版本")
	}
	if manifest.DatabaseSchemaVersion > databaseSchemaVersion {
		return manifest, nil, errors.New("该备份由更高数据库版本的 Meridian 创建，请先升级 Meridian 后再恢复")
	}
	if !manifestIncludesTLS(manifest) {
		for name := range entries {
			if strings.HasPrefix(name, "tls/") {
				return manifest, nil, errors.New("备份清单声明不包含 TLS，但压缩包中存在 TLS 文件")
			}
		}
	}
	if _, ok := entries[backupDatabaseEntry]; !ok {
		return manifest, nil, errors.New("备份缺少数据库")
	}
	declared := make(map[string]bool, len(manifest.Files)+1)
	declared[backupManifestEntry] = true
	for _, name := range manifest.Files {
		if _, ok := backupEntryLimit(name); !ok || name == backupManifestEntry || declared[name] {
			return manifest, nil, errors.New("备份清单文件列表无效")
		}
		declared[name] = true
	}
	if len(declared) != len(entries) {
		return manifest, nil, errors.New("备份清单与文件内容不一致")
	}
	for name := range entries {
		if !declared[name] {
			return manifest, nil, errors.New("备份清单与文件内容不一致")
		}
	}
	return manifest, entries, nil
}

func copyZipEntryToFile(file *zip.File, maxBytes int64, target string) error {
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- target is an internal parser staging path.
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = out.Close()
		if remove {
			_ = os.Remove(target)
		}
	}()
	written, copyErr := io.Copy(out, io.LimitReader(reader, maxBytes+1))
	if copyErr != nil {
		return copyErr
	}
	if written > maxBytes {
		return fmt.Errorf("%s 解压后过大", file.Name)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func ensureRestoreDiskSpace(dbPath string, entries map[string]string) error {
	if strings.TrimSpace(dbPath) == "" || len(entries) == 0 {
		return nil
	}
	var incomingDB, incomingTLS, currentDB int64
	if path := entries[backupDatabaseEntry]; path != "" {
		if info, err := os.Stat(path); err == nil {
			incomingDB = info.Size()
		} else {
			return err
		}
	}
	if info, err := os.Stat(dbPath); err == nil {
		currentDB = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for name, path := range entries {
		if !strings.HasPrefix(name, "tls/") {
			continue
		}
		if info, err := os.Stat(path); err == nil {
			incomingTLS += info.Size()
		} else {
			return err
		}
	}
	if incomingDB <= 0 {
		return errors.New("备份缺少数据库")
	}
	// The restore transaction keeps the uploaded/decrypted artifacts, a staged
	// database, and a rollback copy of the current database alive at once. TLS
	// state is similarly copied into pending and rollback namespaces. Keep a
	// generous fixed margin for SQLite journals and directory metadata.
	required := incomingDB*2 + currentDB + incomingTLS*2 + (64 << 20)
	return ensureDiskSpace(filepath.Dir(dbPath), required)
}

func ensureRestoreDiskSpaceInspection(dbPath string, inspection backupArchiveInspection, encryptedBytes, plainBytes int64) error {
	if strings.TrimSpace(dbPath) == "" {
		return nil
	}
	if inspection.DatabaseBytes <= 0 {
		return errors.New("备份缺少数据库")
	}
	var currentDB int64
	if info, err := os.Stat(dbPath); err == nil {
		currentDB = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Before extraction, account for the encrypted upload, decrypted ZIP,
	// extracted entries, staged database/TLS tree, and rollback copies. This is
	// intentionally conservative: failing closed is preferable to filling the
	// filesystem halfway through a restore.
	if encryptedBytes < 0 || plainBytes < 0 {
		return errors.New("备份大小无效")
	}
	required := inspection.DatabaseBytes*2 + currentDB + inspection.TLSBytes*2 + encryptedBytes + plainBytes + (64 << 20)
	return ensureDiskSpace(filepath.Dir(dbPath), required)
}

func parseBackupZipReader(reader *zip.Reader) (backupManifest, map[string][]byte, error) {
	var manifest backupManifest
	if len(reader.File) < 2 || len(reader.File) > backupMaxFiles {
		return manifest, nil, errors.New("备份文件数量无效")
	}
	entries := make(map[string][]byte, len(reader.File))
	var expanded int64
	for _, file := range reader.File {
		limit, allowed := backupEntryLimit(file.Name)
		if !allowed || filepath.ToSlash(filepath.Clean(file.Name)) != file.Name || strings.HasPrefix(file.Name, "/") {
			return manifest, nil, fmt.Errorf("备份包含不允许的文件: %s", file.Name)
		}
		if _, duplicate := entries[file.Name]; duplicate {
			return manifest, nil, fmt.Errorf("备份包含重复文件: %s", file.Name)
		}
		data, err := readZipEntry(file, limit)
		if err != nil {
			return manifest, nil, err
		}
		expanded += int64(len(data))
		if expanded > backupMaxExpandedBytes {
			return manifest, nil, errors.New("备份解压后总大小超出限制")
		}
		entries[file.Name] = data
	}
	manifestData, ok := entries[backupManifestEntry]
	if !ok {
		return manifest, nil, errors.New("备份缺少清单")
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, nil, errors.New("备份清单无效")
	}
	if manifest.Format != "meridian-backup" || (manifest.FormatVersion != backupFormatVersion && manifest.FormatVersion != backupLegacyFormatVersion) {
		return manifest, nil, errors.New("不支持的备份格式版本")
	}
	if manifest.DatabaseSchemaVersion > databaseSchemaVersion {
		return manifest, nil, errors.New("该备份由更高数据库版本的 Meridian 创建，请先升级 Meridian 后再恢复")
	}
	if !manifestIncludesTLS(manifest) {
		for name := range entries {
			if strings.HasPrefix(name, "tls/") {
				return manifest, nil, errors.New("备份清单声明不包含 TLS，但压缩包中存在 TLS 文件")
			}
		}
	}
	if _, ok := entries[backupDatabaseEntry]; !ok {
		return manifest, nil, errors.New("备份缺少数据库")
	}
	declared := make(map[string]bool, len(manifest.Files)+1)
	declared[backupManifestEntry] = true
	for _, name := range manifest.Files {
		if _, ok := backupEntryLimit(name); !ok || name == backupManifestEntry || declared[name] {
			return manifest, nil, errors.New("备份清单文件列表无效")
		}
		declared[name] = true
	}
	if len(declared) != len(entries) {
		return manifest, nil, errors.New("备份清单与文件内容不一致")
	}
	for name := range entries {
		if !declared[name] {
			return manifest, nil, errors.New("备份清单与文件内容不一致")
		}
	}
	return manifest, entries, nil
}

func validateSQLiteBackup(path string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite 完整性检查失败: %s", result)
	}
	var users, sites int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users); err != nil {
		return errors.New("备份数据库缺少用户数据表")
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sites").Scan(&sites); err != nil {
		return errors.New("备份数据库缺少站点数据表")
	}
	if users < 1 {
		return errors.New("备份中没有管理员账户")
	}
	return nil
}

func sqliteSchemaVersion(path string) (int, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func reencryptRestoredSecrets(path string, oldJWT, oldHeaderKey, newJWT, newHeaderKey []byte) error {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id, target_url, upstream_headers FROM sites WHERE upstream_headers <> '' AND upstream_headers <> '[]'")
	if err != nil {
		return err
	}
	type update struct {
		id  int64
		raw string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var targetURL, raw string
		if err := rows.Scan(&id, &targetURL, &raw); err != nil {
			rows.Close()
			return err
		}
		stored, err := parseStoredUpstreamHeaders(raw)
		if err != nil {
			rows.Close()
			return fmt.Errorf("站点 %d 的自定义请求头无效: %w", id, err)
		}
		target, err := normalizeTargetURL(targetURL)
		if err != nil {
			rows.Close()
			return fmt.Errorf("站点 %d 的目标地址无效: %w", id, err)
		}
		authority := redirectHostKey(target)
		for i := range stored {
			value, err := decryptUpstreamHeaderValue(stored[i].Name, stored[i].Ciphertext, authority, oldHeaderKey)
			if err != nil {
				rows.Close()
				return fmt.Errorf("无法解密站点 %d 的自定义请求头: %w", id, err)
			}
			stored[i].Ciphertext, err = encryptUpstreamHeaderValue(stored[i].Name, value, authority, newHeaderKey)
			if err != nil {
				rows.Close()
				return fmt.Errorf("无法迁移站点 %d 的自定义请求头: %w", id, err)
			}
		}
		encoded, err := json.Marshal(stored)
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, update{id: id, raw: string(encoded)})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := tx.Exec("UPDATE sites SET upstream_headers=? WHERE id=?", item.raw, item.id); err != nil {
			return err
		}
	}
	var telegramCiphertext string
	err = tx.QueryRow("SELECT bot_token_ciphertext FROM telegram_report_settings WHERE id=1").Scan(&telegramCiphertext)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if telegramCiphertext != "" {
		token, err := decryptTelegramBotTokenWithSecret(telegramCiphertext, oldJWT)
		if err != nil {
			return fmt.Errorf("无法解密 Telegram Bot Token: %w", err)
		}
		migrated, err := encryptTelegramBotTokenWithSecret(token, newJWT)
		if err != nil {
			return fmt.Errorf("无法迁移 Telegram Bot Token: %w", err)
		}
		if _, err := tx.Exec("UPDATE telegram_report_settings SET bot_token_ciphertext=? WHERE id=1", migrated); err != nil {
			return err
		}
	}
	hasACMEToken, err := backupSQLiteColumnExists(tx, "panel_settings", "acme_token_ciphertext")
	if err != nil {
		return err
	}
	if hasACMEToken {
		var acmeCiphertext string
		err = tx.QueryRow("SELECT acme_token_ciphertext FROM panel_settings WHERE id=1").Scan(&acmeCiphertext)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if acmeCiphertext != "" {
			token, err := decryptPanelACMETokenWithSecret(acmeCiphertext, oldJWT)
			if err != nil {
				if _, currentErr := decryptPanelACMETokenWithSecret(acmeCiphertext, newJWT); currentErr != nil {
					return fmt.Errorf("无法解密 DNS API Token: %w", err)
				}
			} else {
				migrated, err := encryptPanelACMETokenWithSecret(token, newJWT)
				if err != nil {
					return fmt.Errorf("无法迁移 DNS API Token: %w", err)
				}
				if _, err := tx.Exec("UPDATE panel_settings SET acme_token_ciphertext=? WHERE id=1", migrated); err != nil {
					return err
				}
			}
		}
	}
	hasTMDBToken, err := backupSQLiteColumnExists(tx, "tmdb_settings", "token_ciphertext")
	if err != nil {
		return err
	}
	if hasTMDBToken {
		var tmdbCiphertext string
		err = tx.QueryRow("SELECT token_ciphertext FROM tmdb_settings WHERE id=1").Scan(&tmdbCiphertext)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if tmdbCiphertext != "" {
			token, err := decryptTMDBReadTokenWithSecret(tmdbCiphertext, oldJWT)
			if err != nil {
				if _, currentErr := decryptTMDBReadTokenWithSecret(tmdbCiphertext, newJWT); currentErr != nil {
					return fmt.Errorf("无法解密 TMDB Read Access Token: %w", err)
				}
			} else {
				migrated, err := encryptTMDBReadTokenWithSecret(token, newJWT)
				if err != nil {
					return fmt.Errorf("无法迁移 TMDB Read Access Token: %w", err)
				}
				if _, err := tx.Exec("UPDATE tmdb_settings SET token_ciphertext=? WHERE id=1", migrated); err != nil {
					return err
				}
			}
		}
	}
	if hasProbeSecret, err := backupSQLiteColumnExists(tx, "control_nodes", "probe_secret_ciphertext"); err != nil {
		return err
	} else if hasProbeSecret {
		rows, err := tx.Query("SELECT id,probe_secret_ciphertext FROM control_nodes WHERE probe_secret_ciphertext <> ''")
		if err != nil {
			return err
		}
		type probeUpdate struct {
			id         int64
			ciphertext string
		}
		updates := make([]probeUpdate, 0)
		for rows.Next() {
			var item probeUpdate
			if err := rows.Scan(&item.id, &item.ciphertext); err != nil {
				_ = rows.Close()
				return err
			}
			secret, err := decryptNodeProbeSecretWithSecret(item.ciphertext, oldJWT)
			if err != nil {
				if _, currentErr := decryptNodeProbeSecretWithSecret(item.ciphertext, newJWT); currentErr != nil {
					_ = rows.Close()
					return fmt.Errorf("无法解密节点健康探针密钥: %w", err)
				}
				continue
			}
			migrated, err := encryptNodeProbeSecretWithSecret(secret, newJWT)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("无法迁移节点健康探针密钥: %w", err)
			}
			updates = append(updates, probeUpdate{id: item.id, ciphertext: migrated})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range updates {
			if _, err := tx.Exec("UPDATE control_nodes SET probe_secret_ciphertext=? WHERE id=?", item.ciphertext, item.id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func backupSQLiteColumnExists(queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}, tableName, columnName string) (bool, error) {
	rows, err := queryer.Query("PRAGMA table_info(" + tableName + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	return false, rows.Err()
}

func backupHasJWTProtectedToken(database []byte) (bool, error) {
	dir, err := os.MkdirTemp("", ".meridian-backup-check-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, backupDatabaseEntry)
	if err := writePrivateFileAtomic(path, database); err != nil {
		return false, err
	}
	return backupHasJWTProtectedTokenFile(path)
}

func backupHasJWTProtectedTokenFile(path string) (bool, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return false, err
	}
	defer db.Close()
	hasTelegramColumn, err := backupSQLiteColumnExists(db, "telegram_report_settings", "bot_token_ciphertext")
	if err != nil {
		return false, err
	}
	if hasTelegramColumn {
		var ciphertext string
		if err := db.QueryRow("SELECT bot_token_ciphertext FROM telegram_report_settings WHERE id=1").Scan(&ciphertext); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
		} else if ciphertext != "" {
			return true, nil
		}
	}
	hasTMDBColumn, err := backupSQLiteColumnExists(db, "tmdb_settings", "token_ciphertext")
	if err != nil {
		return false, err
	}
	if !hasTMDBColumn {
		return false, nil
	}
	var ciphertext string
	if err := db.QueryRow("SELECT token_ciphertext FROM tmdb_settings WHERE id=1").Scan(&ciphertext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return ciphertext != "", nil
}

func readBackupPanelSettings(db *sql.DB) (*backupPanelSettings, error) {
	if db == nil {
		return nil, errors.New("当前数据库不可用")
	}
	if err := ensureBackupPanelACMEColumns(db); err != nil {
		return nil, err
	}
	var settings backupPanelSettings
	err := db.QueryRow(`SELECT panel_domain, route_domain, listen_port, tls_enabled, configured,
		acme_email, acme_dns_provider, acme_token_ciphertext, acme_staging FROM panel_settings WHERE id=1`).Scan(
		&settings.PanelDomain, &settings.RouteDomain, &settings.ListenPort, &settings.TLSEnabled, &settings.Configured,
		&settings.ACMEEmail, &settings.ACMEDNSProvider, &settings.ACMETokenCiphertext, &settings.ACMEStaging,
	)
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

func ensureBackupPanelACMEColumns(db *sql.DB) error {
	columns := []struct {
		name       string
		definition string
	}{
		{"acme_email", "TEXT NOT NULL DEFAULT ''"},
		{"acme_dns_provider", "TEXT NOT NULL DEFAULT 'cloudflare'"},
		{"acme_token_ciphertext", "TEXT NOT NULL DEFAULT ''"},
		{"acme_staging", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		exists, err := backupSQLiteColumnExists(db, "panel_settings", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := db.Exec("ALTER TABLE panel_settings ADD COLUMN " + column.name + " " + column.definition); err != nil {
			return err
		}
	}
	return nil
}

func preserveBackupPanelSettings(path string, settings *backupPanelSettings) error {
	if settings == nil {
		return errors.New("缺少目标服务器 TLS 设置")
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureBackupPanelACMEColumns(db); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE panel_settings SET panel_domain=?, route_domain=?, listen_port=?, tls_enabled=?, configured=?,
		acme_email=?, acme_dns_provider=?, acme_token_ciphertext=?, acme_staging=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`,
		settings.PanelDomain, settings.RouteDomain, settings.ListenPort, settings.TLSEnabled, settings.Configured,
		settings.ACMEEmail, settings.ACMEDNSProvider, settings.ACMETokenCiphertext, settings.ACMEStaging)
	return err
}

func reconcileRestoredSiteIngress(path string, hostIngressAvailable bool) (int64, error) {
	if hostIngressAvailable {
		return 0, nil
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE sites SET public_host='', path_prefix='', ingress_mode=?, enabled=0, updated_at=CURRENT_TIMESTAMP WHERE ingress_mode IN (?, ?)`, ingressModeUnset, ingressModeHost, ingressModeBoth)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func writeRestorePendingFromPaths(dbPath string, manifest backupManifest, entries map[string]string, targetJWT, targetHeaderKey []byte, preservedPanelSettings *backupPanelSettings, targetHostIngressWithoutTLS bool) (int64, error) {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return 0, errors.New("当前数据库模式不支持恢复")
	}
	oldJWT, err := base64.RawStdEncoding.DecodeString(manifest.JWTSecret)
	if err != nil || len(oldJWT) < 32 {
		return 0, errors.New("备份缺少有效的 JWT 密钥迁移信息")
	}
	oldHeaderKey, err := base64.RawStdEncoding.DecodeString(manifest.UpstreamHeaderKey)
	if err != nil {
		return 0, errors.New("备份中的上游请求头密钥无效")
	}
	pending := dbPath + backupPendingSuffix
	tmp, err := os.MkdirTemp(filepath.Dir(dbPath), ".meridian-restore-stage-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	databasePath := filepath.Join(tmp, backupDatabaseEntry)
	if err := copyPrivateFile(entries[backupDatabaseEntry], databasePath); err != nil {
		return 0, err
	}
	if err := validateSQLiteBackup(databasePath); err != nil {
		return 0, err
	}
	schemaVersion, err := sqliteSchemaVersion(databasePath)
	if err != nil {
		return 0, fmt.Errorf("读取备份数据库版本: %w", err)
	}
	if schemaVersion > databaseSchemaVersion || manifest.DatabaseSchemaVersion > databaseSchemaVersion {
		return 0, errors.New("该备份由更高数据库版本的 Meridian 创建，请先升级 Meridian 后再恢复")
	}
	if !manifestIncludesTLS(manifest) {
		if err := preserveBackupPanelSettings(databasePath, preservedPanelSettings); err != nil {
			return 0, fmt.Errorf("保留目标服务器 TLS 设置: %w", err)
		}
	}
	stagedDB, err := sql.Open("sqlite", databasePath+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	stagedPanelSettings, err := readBackupPanelSettings(stagedDB)
	_ = stagedDB.Close()
	if err != nil {
		return 0, fmt.Errorf("读取恢复后的面板设置: %w", err)
	}
	hostIngressAvailable := stagedPanelSettings.RouteDomain != "" && targetHostIngressWithoutTLS
	if stagedPanelSettings.RouteDomain != "" && stagedPanelSettings.TLSEnabled == 1 {
		if !manifestIncludesTLS(manifest) {
			hostIngressAvailable = true
		} else {
			_, hasCertificate := entries[backupTLSCertificate]
			_, hasPrivateKey := entries[backupTLSPrivateKey]
			if !hasCertificate || !hasPrivateKey {
				return 0, errors.New("备份启用了 TLS，但缺少证书或私钥")
			}
			hostIngressAvailable = true
		}
	}
	resetIngressCount, err := reconcileRestoredSiteIngress(databasePath, hostIngressAvailable)
	if err != nil {
		return 0, fmt.Errorf("迁移站点入口配置: %w", err)
	}
	if err := reencryptRestoredSecrets(databasePath, oldJWT, oldHeaderKey, targetJWT, targetHeaderKey); err != nil {
		return 0, err
	}
	if err := validateSQLiteBackup(databasePath); err != nil {
		return 0, err
	}
	for name, source := range entries {
		if !strings.HasPrefix(name, "tls/") {
			continue
		}
		if err := copyPrivateFile(source, filepath.Join(tmp, filepath.FromSlash(name))); err != nil {
			return 0, err
		}
	}
	markerData, err := json.Marshal(restoreMarker{Files: manifest.Files, IncludeTLS: boolPointer(manifestIncludesTLS(manifest))})
	if err != nil {
		return 0, err
	}
	if err := writePrivateFileAtomic(filepath.Join(tmp, "restore.json"), markerData); err != nil {
		return 0, err
	}
	if err := os.RemoveAll(pending); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, pending); err != nil {
		return 0, err
	}
	return resetIngressCount, nil
}

// writeRestorePending keeps the byte-slice API for existing callers and unit
// tests. HTTP restore uses writeRestorePendingFromPaths to avoid retaining the
// whole decrypted archive in memory.
func writeRestorePending(dbPath string, manifest backupManifest, entries map[string][]byte, targetJWT, targetHeaderKey []byte, preservedPanelSettings *backupPanelSettings, targetHostIngressWithoutTLS bool) (int64, error) {
	stage, err := os.MkdirTemp("", ".meridian-restore-entries-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(stage)
	paths := make(map[string]string, len(entries))
	for name, data := range entries {
		if _, allowed := backupEntryLimit(name); !allowed {
			return 0, fmt.Errorf("备份包含不允许的文件: %s", name)
		}
		path := filepath.Join(stage, fmt.Sprintf("%08d.entry", len(paths)))
		if err := writePrivateFileAtomic(path, data); err != nil {
			return 0, err
		}
		paths[name] = path
	}
	return writeRestorePendingFromPaths(dbPath, manifest, paths, targetJWT, targetHeaderKey, preservedPanelSettings, targetHostIngressWithoutTLS)
}

func copyPrivateFile(source, target string) error {
	if filepath.Clean(source) == filepath.Clean(target) {
		return errors.New("source and target must differ")
	}
	src, err := os.Open(source) // #nosec G304 G703 -- source is always a path generated from the private restore directory and an allowlisted entry.
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil { // #nosec G703 -- target is an internally generated private path.
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".meridian-copy-*") // #nosec G703 -- directory is an internally generated private path.
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName) // #nosec G703 -- temporary file was created by this function.
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil { // #nosec G703 -- both paths are private restore paths.
		return err
	}
	removeTemp = false
	return syncDirectory(filepath.Dir(target))
}

func isPanelCertificatePairEntry(entry string) bool {
	return entry == backupTLSCertificate || entry == backupTLSPrivateKey
}

func restorePanelCertificatePair(dbPath, sourceDir string) (bool, error) {
	certSource := filepath.Join(sourceDir, filepath.FromSlash(backupTLSCertificate))
	keySource := filepath.Join(sourceDir, filepath.FromSlash(backupTLSPrivateKey))
	certPEM, certErr := os.ReadFile(certSource) // #nosec G304 G703 -- source is an allowlisted restore directory and fixed TLS entry.
	keyPEM, keyErr := os.ReadFile(keySource)    // #nosec G304 G703 -- source is an allowlisted restore directory and fixed TLS entry.
	if errors.Is(certErr, os.ErrNotExist) || errors.Is(keyErr, os.ErrNotExist) {
		return false, nil
	}
	if certErr != nil {
		return false, certErr
	}
	if keyErr != nil {
		return false, keyErr
	}
	manager := newPanelCertificateManager(dbPath, nil)
	certTarget, keyTarget := manager.panelAtomicPairPaths()
	if certTarget == "" || keyTarget == "" {
		return false, errors.New("panel TLS certificate storage is unavailable")
	}
	return true, installCertificatePairAtomic(certTarget, keyTarget, certPEM, keyPEM)
}

func targetTLSPath(dbPath, entry string) string {
	if tlsStateDir(dbPath) == "" {
		return ""
	}
	tlsDir := tlsStateDir(dbPath)
	panelCurrentDir := filepath.Join(tlsDir, ".panel-current")
	switch entry {
	case backupTLSCertificate:
		return filepath.Join(panelCurrentDir, "fullchain.pem")
	case backupTLSPrivateKey:
		return filepath.Join(panelCurrentDir, "privkey.pem")
	case backupTLSEnabled:
		return filepath.Join(tlsDir, "enabled")
	case backupACMEAccount:
		return filepath.Join(tlsDir, "acme-account.pem")
	case backupACMEAccountStaging:
		return filepath.Join(tlsDir, "acme-account-staging.pem")
	default:
		if !strings.HasPrefix(entry, backupTLSEdgeNodesPrefix) {
			return ""
		}
		parts := strings.Split(strings.TrimPrefix(entry, backupTLSEdgeNodesPrefix), "/")
		if len(parts) != 2 || (parts[1] != "fullchain.pem" && parts[1] != "privkey.pem") {
			return ""
		}
		if _, ok := backupEntryLimit(entry); !ok {
			return ""
		}
		root := edgeNodeTLSRoot(dbPath)
		if root == "" {
			return ""
		}
		return filepath.Join(root, parts[0], "current", parts[1])
	}
}

type tlsRestoreScope struct {
	OwnedRoots      []string `json:"owned_roots,omitempty"`
	ExactPaths      []string `json:"exact_paths,omitempty"`
	GenerationRoots []string `json:"generation_roots,omitempty"`
}

type tlsSnapshotPath struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

type tlsNamespaceSnapshot struct {
	// Roots is retained for rollback compatibility with v1.9.34 snapshots.
	Roots []string          `json:"roots,omitempty"`
	Scope tlsRestoreScope   `json:"scope,omitempty"`
	Exact []tlsSnapshotPath `json:"exact,omitempty"`
}

func appendUniqueTLSPath(values []string, seen map[string]struct{}, value string) []string {
	value = filepath.Clean(strings.TrimSpace(value))
	if value == "." || value == "" {
		return values
	}
	if _, ok := seen[value]; ok {
		return values
	}
	seen[value] = struct{}{}
	return append(values, value)
}

// pathWithin reports lexical containment without following symlinks. It is
// used before copying/removing managed directories so a rollback destination
// can never be created inside the source namespace (or vice versa).
func pathWithin(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func tlsPathsOverlap(first, second string) bool {
	if pathWithin(first, second) || pathWithin(second, first) {
		return true
	}
	canonicalFirst, firstErr := canonicalConfiguredPath(first)
	canonicalSecond, secondErr := canonicalConfiguredPath(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	return pathWithin(canonicalFirst, canonicalSecond) || pathWithin(canonicalSecond, canonicalFirst)
}

func managedTLSRestoreScope(dbPath string) tlsRestoreScope {
	scope := tlsRestoreScope{}
	ownedSeen := make(map[string]struct{}, 2)
	stateDir := tlsStateDir(dbPath)
	if stateDir != "" {
		// TLS_STATE_DIR is the only directory Meridian may replace wholesale.
		// Operator-provided cert/key files are intentionally excluded.
		scope.OwnedRoots = appendUniqueTLSPath(scope.OwnedRoots, ownedSeen, stateDir)
	}
	// Keep the legacy per-node namespace in scope during migration, but only
	// the explicitly named edge-nodes child is owned; the external parent and
	// certificate/key files remain operator-owned.
	if edgeRoot := edgeNodeTLSRoot(dbPath); edgeRoot != "" {
		coveredByState := false
		for _, root := range scope.OwnedRoots {
			if pathWithin(root, edgeRoot) {
				coveredByState = true
				break
			}
		}
		if !coveredByState {
			scope.OwnedRoots = appendUniqueTLSPath(scope.OwnedRoots, ownedSeen, edgeRoot)
		}
	}
	return scope
}

// managedTLSRoots remains as a compatibility helper for older tests/callers,
// but only returns explicitly owned Meridian roots. Custom certificate parent
// directories are deliberately excluded.
func managedTLSRoots(dbPath string) []string {
	return managedTLSRestoreScope(dbPath).OwnedRoots
}

func copyTLSNamespaceTree(source, target string) error {
	source = filepath.Clean(source)
	target = filepath.Clean(target)
	if tlsPathsOverlap(source, target) {
		return errors.New("TLS snapshot source and target must not overlap")
	}
	info, err := os.Lstat(source) // #nosec G703 -- source is an internally derived Meridian TLS namespace root.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("TLS namespace root is not a directory")
	}
	if err := os.MkdirAll(target, 0o700); err != nil { // #nosec G703 -- target is an internally generated rollback namespace path.
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error { // #nosec G703 G122 -- source and target are private Meridian TLS roots; symlinks are copied without following them.
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		destination := filepath.Join(target, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil { // #nosec G703 -- destination is beneath the generated rollback root.
				return err
			}
			_ = os.Remove(destination)           // #nosec G703 -- destination is beneath the generated rollback root.
			return os.Symlink(link, destination) // #nosec G703 G122 -- destination is beneath the generated rollback root and link text is copied verbatim.
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700) // #nosec G703 -- destination is beneath the generated rollback root.
		}
		return copyPrivateFile(path, destination)
	})
}

func copyManagedTLSPath(source, target string) (bool, error) {
	source = filepath.Clean(source)
	target = filepath.Clean(target)
	info, err := os.Lstat(source) // #nosec G703 -- source is an internally derived TLS path from the configured scope.
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		if err := copyTLSNamespaceTree(source, target); err != nil {
			return false, err
		}
		return true, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(source)
		if err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil { // #nosec G703 -- target is beneath the private rollback namespace.
			return false, err
		}
		_ = os.Remove(target)                            // #nosec G703 -- target is an internally generated rollback path.
		if err := os.Symlink(link, target); err != nil { // #nosec G703 G122 -- link text is copied verbatim without following the link.
			return false, err
		}
		return true, nil
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("TLS snapshot path must be a regular file, directory, or symlink")
	}
	if err := copyPrivateFile(source, target); err != nil {
		return false, err
	}
	return true, nil
}

func copyTLSGenerationEntries(source, target string) error {
	entries, err := os.ReadDir(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "generation-") {
			continue
		}
		if err := os.MkdirAll(target, 0o700); err != nil { // #nosec G703 -- target is beneath the private rollback namespace.
			return err
		}
		if _, err := copyManagedTLSPath(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeTLSGenerationEntries(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "generation-") {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil { // #nosec G703 -- entry names are constrained by the generation prefix and root is configured TLS state.
				return err
			}
		}
	}
	return nil
}

func removeTLSRestoreScope(scope tlsRestoreScope) error {
	for _, root := range scope.OwnedRoots {
		if err := os.RemoveAll(root); err != nil { // #nosec G703 -- root is an explicitly owned Meridian TLS namespace.
			return err
		}
	}
	for _, path := range scope.ExactPaths {
		info, err := os.Lstat(path) // #nosec G703 -- path is an explicitly tracked certificate/account file or pointer.
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.IsDir() && filepath.Base(path) != ".panel-current" {
			return fmt.Errorf("TLS exact path unexpectedly points to a directory: %s", path)
		}
		if err := os.RemoveAll(path); err != nil { // #nosec G703 -- path is an explicitly tracked certificate/account file or pointer.
			return err
		}
	}
	for _, root := range scope.GenerationRoots {
		if err := removeTLSGenerationEntries(root); err != nil {
			return err
		}
	}
	return nil
}

func validateTLSRestoreScope(dbPath string, scope tlsRestoreScope) error {
	if strings.TrimSpace(dbPath) == "" {
		return nil
	}
	protectedBase, err := filepath.Abs(dbPath)
	if err != nil {
		return fmt.Errorf("解析数据库路径: %w", err)
	}
	protected := []string{protectedBase, protectedBase + backupPendingSuffix, protectedBase + backupAppliedSuffix, protectedBase + backupRollbackSuffix}
	managed := append(append([]string{}, scope.OwnedRoots...), scope.ExactPaths...)
	managed = append(managed, scope.GenerationRoots...)
	for _, path := range managed {
		for _, blocked := range protected {
			if tlsPathsOverlap(path, blocked) {
				return fmt.Errorf("TLS 恢复路径不能覆盖数据库或恢复工作目录: %s", path)
			}
		}
	}
	for index, first := range managed {
		if strings.TrimSpace(first) == "" {
			return errors.New("TLS 恢复路径为空")
		}
		for _, second := range managed[index+1:] {
			if tlsPathsOverlap(first, second) {
				return fmt.Errorf("TLS 恢复路径互相重叠: %s", first)
			}
		}
	}
	return nil
}

func snapshotTLSNamespace(dbPath, rollback string) error {
	if err := validateTLSPathConfiguration(dbPath); err != nil {
		return err
	}
	scope := managedTLSRestoreScope(dbPath)
	if err := validateTLSRestoreScope(dbPath, scope); err != nil {
		return err
	}
	if len(scope.OwnedRoots) == 0 && len(scope.ExactPaths) == 0 && len(scope.GenerationRoots) == 0 {
		return nil
	}
	base := filepath.Join(rollback, "tls-tree")
	if err := os.RemoveAll(base); err != nil { // #nosec G703 -- base is beneath the private rollback directory created by Meridian.
		return err
	}
	if err := os.MkdirAll(base, 0o700); err != nil { // #nosec G703 -- base is beneath the private rollback directory created by Meridian.
		return err
	}
	for index, root := range scope.OwnedRoots {
		if err := copyTLSNamespaceTree(root, filepath.Join(base, "owned", fmt.Sprintf("%d", index))); err != nil {
			return fmt.Errorf("备份 TLS 目录 %s: %w", root, err)
		}
	}
	for index, path := range scope.ExactPaths {
		present, err := copyManagedTLSPath(path, filepath.Join(base, "exact", fmt.Sprintf("%d", index)))
		if err != nil {
			return fmt.Errorf("备份 TLS 路径 %s: %w", path, err)
		}
		if !present {
			// Keep the destination namespace deterministic even for absent files.
			_ = os.RemoveAll(filepath.Join(base, "exact", fmt.Sprintf("%d", index))) // #nosec G703 -- generated rollback path.
		}
	}
	for index, root := range scope.GenerationRoots {
		if err := copyTLSGenerationEntries(root, filepath.Join(base, "generations", fmt.Sprintf("%d", index))); err != nil {
			return fmt.Errorf("备份 TLS generation 目录 %s: %w", root, err)
		}
	}
	exact := make([]tlsSnapshotPath, len(scope.ExactPaths))
	for index, path := range scope.ExactPaths {
		_, statErr := os.Lstat(path) // #nosec G703 -- path is an explicitly tracked TLS path.
		exact[index] = tlsSnapshotPath{Path: path, Present: statErr == nil}
	}
	snapshot := tlsNamespaceSnapshot{Scope: scope, Exact: exact}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return writePrivateFileAtomic(filepath.Join(rollback, "tls-namespace.json"), data)
}

func removeManagedTLSNamespace(dbPath string) error {
	if err := validateTLSPathConfiguration(dbPath); err != nil {
		return err
	}
	scope := managedTLSRestoreScope(dbPath)
	if err := validateTLSRestoreScope(dbPath, scope); err != nil {
		return err
	}
	return removeTLSRestoreScope(scope)
}

func restoreTLSNamespaceSnapshot(dbPath, rollback string) error {
	if err := validateTLSPathConfiguration(dbPath); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(rollback, "tls-namespace.json")) // #nosec G304 G703 -- rollback is a private directory created by Meridian.
	if err != nil {
		return err
	}
	var snapshot tlsNamespaceSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	currentScope := managedTLSRestoreScope(dbPath)
	scope := snapshot.Scope
	if len(scope.OwnedRoots) == 0 && len(scope.ExactPaths) == 0 && len(scope.GenerationRoots) == 0 && len(snapshot.Roots) > 0 {
		// v1.9.34 snapshots may only be safely replayed when their roots are
		// the default DB-local TLS namespace. Never trust a legacy custom
		// parent directory as an owned root.
		defaultRoot, absErr := filepath.Abs(filepath.Join(filepath.Dir(dbPath), "tls"))
		if absErr != nil {
			return fmt.Errorf("解析旧版 TLS 回滚目录: %w", absErr)
		}
		for _, root := range snapshot.Roots {
			rootAbsolute, rootErr := filepath.Abs(root)
			if rootErr != nil || filepath.Clean(rootAbsolute) != filepath.Clean(defaultRoot) {
				return errors.New("旧版 TLS 回滚清单包含不安全的自定义目录")
			}
		}
		scope = tlsRestoreScope{OwnedRoots: append([]string(nil), snapshot.Roots...)}
	}
	if !sameTLSRestoreScope(scope, currentScope) {
		return errors.New("TLS 回滚清单与当前配置不一致")
	}
	if err := validateTLSRestoreScope(dbPath, currentScope); err != nil {
		return err
	}
	if err := removeTLSRestoreScope(currentScope); err != nil {
		return err
	}
	base := filepath.Join(rollback, "tls-tree")
	for index, root := range scope.OwnedRoots {
		if err := copyTLSNamespaceTree(filepath.Join(base, "owned", fmt.Sprintf("%d", index)), root); err != nil {
			return err
		}
	}
	for index, path := range scope.ExactPaths {
		if index >= len(snapshot.Exact) || !snapshot.Exact[index].Present {
			continue
		}
		if _, err := copyManagedTLSPath(filepath.Join(base, "exact", fmt.Sprintf("%d", index)), path); err != nil {
			return err
		}
	}
	for index, root := range scope.GenerationRoots {
		entries, err := os.ReadDir(filepath.Join(base, "generations", fmt.Sprintf("%d", index)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "generation-") {
				continue
			}
			if _, err := copyManagedTLSPath(filepath.Join(base, "generations", fmt.Sprintf("%d", index), entry.Name()), filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameTLSRestoreScope(a, b tlsRestoreScope) bool {
	clean := func(values []string) []string {
		result := make([]string, len(values))
		for index, value := range values {
			result[index] = filepath.Clean(value)
		}
		return result
	}
	return slicesEqual(clean(a.OwnedRoots), clean(b.OwnedRoots)) && slicesEqual(clean(a.ExactPaths), clean(b.ExactPaths)) && slicesEqual(clean(a.GenerationRoots), clean(b.GenerationRoots))
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func replaceTLSNamespaceWithPending(dbPath string) error {
	if err := removeManagedTLSNamespace(dbPath); err != nil {
		return err
	}
	return nil
}

func applyPendingRestore(dbPath string) (*restoreAppliedState, error) {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return nil, nil
	}
	pending := dbPath + backupPendingSuffix
	appliedMarker := dbPath + backupAppliedSuffix
	rollback := dbPath + backupRollbackSuffix
	if _, err := os.Stat(appliedMarker); err == nil { // #nosec G703 G304 -- all paths are derived from the administrator-controlled database path and fixed restore suffixes.
		if err := rollbackRestoreFiles(dbPath, rollback); err != nil {
			return nil, fmt.Errorf("回滚上次未完成的恢复: %w", err)
		}
		_ = os.Remove(appliedMarker) // #nosec G703 G304 -- fixed suffix path derived from the configured database path.
		_ = os.RemoveAll(rollback)   // #nosec G703 G304 -- fixed suffix path derived from the configured database path.
		// A crash after the staged database was moved leaves an incomplete
		// pending directory. The old installation is authoritative after the
		// rollback; discard that stage instead of trying to apply it again.
		_ = os.RemoveAll(pending) // #nosec G703 G304 -- fixed suffix path derived from the configured database path.
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Stat(pending); errors.Is(err, os.ErrNotExist) { // #nosec G703 -- pending is the fixed restore suffix derived from the configured database path.
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	markerData, err := os.ReadFile(filepath.Join(pending, "restore.json")) // #nosec G304 G703 -- pending is the fixed restore staging directory.
	if err != nil {
		return nil, fmt.Errorf("读取待恢复清单: %w", err)
	}
	var marker restoreMarker
	if err := json.Unmarshal(markerData, &marker); err != nil {
		return nil, fmt.Errorf("待恢复清单损坏: %w", err)
	}
	if err := os.RemoveAll(rollback); err != nil { // #nosec G703 G304 -- fixed rollback suffix path.
		return nil, err
	}
	if err := os.MkdirAll(rollback, 0o700); err != nil { // #nosec G703 G304 -- fixed rollback suffix path.
		return nil, err
	}
	rollbackReady := false
	committed := false
	defer func() {
		if rollbackReady && !committed {
			_ = rollbackRestoreFiles(dbPath, rollback)
			_ = os.Remove(dbPath + backupAppliedSuffix) // #nosec G703 G304 -- fixed restore marker suffix.
			_ = os.RemoveAll(rollback)                  // #nosec G703 G304 -- fixed rollback suffix path.
			_ = os.RemoveAll(pending)                   // #nosec G703 G304 -- fixed pending suffix path.
		}
	}()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		source := dbPath + suffix
		if _, err := os.Stat(source); err == nil { // #nosec G703 G304 -- source is the configured database path plus a fixed SQLite suffix.
			if err := copyPrivateFile(source, filepath.Join(rollback, backupDatabaseEntry+suffix)); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	// Persist the scope before the first destructive action. If startup is
	// interrupted later, automatic rollback must know whether TLS participated.
	if err := writePrivateFileAtomic(filepath.Join(rollback, "restore.json"), markerData); err != nil { // #nosec G703 G304 -- rollback is the fixed restore directory.
		return nil, err
	}
	rollbackReady = true
	if markerIncludesTLS(marker) {
		if err := snapshotTLSNamespace(dbPath, rollback); err != nil {
			return nil, err
		}
	}
	// From this point on every destructive change is recoverable. The marker is
	// written before replacing live files so a process interruption at any later
	// instruction causes the next startup to restore the complete old snapshot.
	if err := writePrivateFileAtomic(appliedMarker, []byte("pending validation\n")); err != nil { // #nosec G703 G304 -- fixed restore marker suffix.
		return nil, err
	}
	if markerIncludesTLS(marker) {
		if err := replaceTLSNamespaceWithPending(dbPath); err != nil {
			return nil, err
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 G304 -- fixed SQLite sidecar suffix.
			return nil, err
		}
	}
	if err := os.Rename(filepath.Join(pending, backupDatabaseEntry), dbPath); err != nil { // #nosec G703 G304 -- source is the fixed database archive entry in pending.
		return nil, err
	}
	if markerIncludesTLS(marker) {
		panelPairRestored := false
		var pairErr error
		panelPairRestored, pairErr = restorePanelCertificatePair(dbPath, pending)
		if pairErr != nil {
			return nil, pairErr
		}
		for _, entry := range marker.Files {
			if !strings.HasPrefix(entry, "tls/") {
				continue
			}
			if isPanelCertificatePairEntry(entry) && panelPairRestored {
				continue
			}
			target := targetTLSPath(dbPath, entry)
			if target == "" {
				continue
			}
			if err := copyPrivateFile(filepath.Join(pending, filepath.FromSlash(entry)), target); err != nil { // #nosec G703 G304 -- entry is allowlisted and target is a fixed TLS path.
				return nil, err
			}
		}
	}
	if err := os.RemoveAll(pending); err != nil { // #nosec G703 G304 -- fixed pending suffix path.
		return nil, err
	}
	committed = true
	return &restoreAppliedState{RollbackDir: rollback}, nil
}

func rollbackRestoreFiles(dbPath, rollback string) error {
	if rollback == "" {
		return errors.New("恢复回滚目录为空")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix) // #nosec G703 G304 -- fixed SQLite sidecar suffix.
		source := filepath.Join(rollback, backupDatabaseEntry+suffix)
		if _, err := os.Stat(source); err == nil { // #nosec G703 G304 -- source is the fixed rollback database entry.
			if err := os.Rename(source, dbPath+suffix); err != nil {
				return err
			}
		} else if suffix == "" {
			return errors.New("恢复回滚副本缺少数据库")
		}
	}
	if restoreDirectoryIncludesTLS(rollback) {
		if _, err := os.Stat(filepath.Join(rollback, "tls-namespace.json")); err == nil { // #nosec G703 -- rollback is the private fixed restore directory.
			return restoreTLSNamespaceSnapshot(dbPath, rollback)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		panelPairRestored, pairErr := restorePanelCertificatePair(dbPath, rollback)
		if pairErr != nil {
			return pairErr
		}
		for _, entry := range restoreDirectoryTLSEntries(rollback) {
			if isPanelCertificatePairEntry(entry) && panelPairRestored {
				continue
			}
			target := targetTLSPath(dbPath, entry)
			if target == "" {
				continue
			}
			_ = os.Remove(target) // #nosec G703 G304 -- target is derived from the fixed TLS allowlist.
			source := filepath.Join(rollback, filepath.FromSlash(entry))
			if _, err := os.Stat(source); err == nil { // #nosec G703 G304 -- source is an allowlisted rollback TLS entry.
				if err := copyPrivateFile(source, target); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func rollbackAppliedRestore(dbPath string, state *restoreAppliedState) error {
	if state == nil {
		return nil
	}
	if err := rollbackRestoreFiles(dbPath, state.RollbackDir); err != nil {
		return err
	}
	_ = os.Remove(dbPath + backupAppliedSuffix) // #nosec G703 G304 -- fixed restore marker suffix.
	return os.RemoveAll(state.RollbackDir)      // #nosec G703 G304 -- rollback directory was created by Meridian.
}

func finalizeAppliedRestore(dbPath string) error {
	if err := os.Remove(dbPath + backupAppliedSuffix); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 G304 -- fixed restore marker suffix.
		return err
	}
	return os.RemoveAll(dbPath + backupRollbackSuffix) // #nosec G703 G304 -- fixed rollback suffix path.
}

func (a *App) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		Password   string `json:"password"`
		IncludeTLS *bool  `json:"include_tls"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	a.backupMu.Lock()
	defer a.backupMu.Unlock()
	includeTLS := false
	if request.IncludeTLS != nil {
		includeTLS = *request.IncludeTLS
	}
	artifact, err := a.buildBackupToFile(request.Password, includeTLS)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer artifact.Cleanup()
	file, err := os.Open(artifact.Path) // #nosec G304 -- artifact is a private temporary backup file created above.
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "备份文件读取失败")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="meridian-backup-%s.mrbak"`, time.Now().Format("20060102-150405")))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", artifact.Size))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (a *App) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.dbPath == "" || a.dbPath == ":memory:" || strings.HasPrefix(a.dbPath, "file:") {
		a.jsonErr(w, http.StatusConflict, "当前数据库模式不支持恢复")
		return
	}
	// Parse multipart parts directly into the private restore directory. Using
	// ParseMultipartForm would spill large file parts to the system temp
	// directory and retain form metadata in memory before we can perform a disk
	// preflight.
	r.Body = http.MaxBytesReader(w, r.Body, backupMaxUploadBytes+(4<<20))
	restoreTemp, err := os.MkdirTemp(filepath.Dir(a.dbPath), ".meridian-restore-upload-*")
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "恢复暂存目录创建失败")
		return
	}
	defer os.RemoveAll(restoreTemp)
	encryptedPath := filepath.Join(restoreTemp, "backup.mrbak")
	reader, err := r.MultipartReader()
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, "上传文件过大或表单无效")
		return
	}
	var password, confirmation string
	var readBytes int64
	var havePassword, haveConfirmation, haveBackup bool
	var encrypted *os.File
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			a.jsonErr(w, http.StatusBadRequest, "上传文件过大或表单无效")
			return
		}
		name := part.FormName()
		if name == "password" || name == "confirm" {
			if (name == "password" && havePassword) || (name == "confirm" && haveConfirmation) {
				_ = part.Close()
				a.jsonErr(w, http.StatusBadRequest, "表单字段重复")
				return
			}
			value, valueErr := io.ReadAll(io.LimitReader(part, backupMaxPasswordBytes+1))
			_ = part.Close()
			if valueErr != nil || len(value) > backupMaxPasswordBytes {
				a.jsonErr(w, http.StatusBadRequest, "备份密码或确认信息无效")
				return
			}
			if name == "password" {
				password, havePassword = string(value), true
			} else {
				confirmation, haveConfirmation = string(value), true
			}
			continue
		}
		if name != "backup" || haveBackup {
			_ = part.Close()
			a.jsonErr(w, http.StatusBadRequest, "表单字段无效")
			return
		}
		haveBackup = true
		var openErr error
		encrypted, openErr = os.OpenFile(encryptedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- encryptedPath is inside a private restore directory.
		if openErr != nil {
			_ = part.Close()
			a.jsonErr(w, http.StatusInternalServerError, "恢复暂存文件创建失败")
			return
		}
		readBytes, err = io.Copy(encrypted, io.LimitReader(part, backupMaxUploadBytes+1))
		closeErr := encrypted.Close()
		_ = part.Close()
		if err != nil || closeErr != nil || readBytes > backupMaxUploadBytes {
			a.jsonErr(w, http.StatusBadRequest, "备份文件读取失败或超过 256 MiB")
			return
		}
	}
	if !havePassword || !haveConfirmation || confirmation != "恢复" {
		a.jsonErr(w, http.StatusBadRequest, "请输入“恢复”确认操作")
		return
	}
	if !haveBackup {
		a.jsonErr(w, http.StatusBadRequest, "请选择 Meridian 备份文件")
		return
	}
	if err := ensureDiskSpace(restoreTemp, readBytes*3+64<<20); err != nil {
		a.jsonErr(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	plainPath := filepath.Join(restoreTemp, "archive.zip")
	plainFile, err := os.OpenFile(plainPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- plainPath is inside a private restore directory.
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "恢复解密文件创建失败")
		return
	}
	encrypted, err = os.Open(encryptedPath) // #nosec G304 -- encryptedPath is inside a private restore directory.
	if err != nil {
		_ = plainFile.Close()
		a.jsonErr(w, http.StatusInternalServerError, "恢复暂存文件读取失败")
		return
	}
	prefix := make([]byte, len(backupMagicV2))
	_, prefixErr := io.ReadFull(encrypted, prefix)
	if _, err := encrypted.Seek(0, io.SeekStart); err != nil {
		_ = encrypted.Close()
		_ = plainFile.Close()
		a.jsonErr(w, http.StatusInternalServerError, "恢复暂存文件定位失败")
		return
	}
	var decryptErr error
	if prefixErr == nil && string(prefix) == backupMagicV2 {
		_, decryptErr = openBackupV2Reader(encrypted, password, plainFile)
	} else {
		legacyPayload, readErr := io.ReadAll(io.LimitReader(encrypted, backupMaxUploadBytes+1))
		if readErr != nil || int64(len(legacyPayload)) > backupMaxUploadBytes {
			decryptErr = errors.New("备份文件读取失败或超过 256 MiB")
		} else {
			var plain []byte
			plain, decryptErr = openBackup(legacyPayload, password)
			if decryptErr == nil {
				var written int
				written, decryptErr = plainFile.Write(plain)
				if decryptErr == nil && written != len(plain) {
					decryptErr = io.ErrShortWrite
				}
			}
		}
	}
	_ = encrypted.Close()
	if closeErr := plainFile.Close(); decryptErr == nil && closeErr != nil {
		decryptErr = closeErr
	}
	if decryptErr != nil {
		a.jsonErr(w, http.StatusBadRequest, decryptErr.Error())
		return
	}
	plainInfo, statErr := os.Stat(plainPath)
	if statErr != nil {
		a.jsonErr(w, http.StatusInternalServerError, "恢复解密文件检查失败")
		return
	}
	inspection, inspectErr := inspectBackupArchiveFile(plainPath)
	if inspectErr != nil {
		a.jsonErr(w, http.StatusBadRequest, inspectErr.Error())
		return
	}
	if err := ensureRestoreDiskSpaceInspection(a.dbPath, inspection, readBytes, plainInfo.Size()); err != nil {
		a.jsonErr(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	a.backupMu.Lock()
	defer a.backupMu.Unlock()
	entriesDir := filepath.Join(restoreTemp, "entries")
	manifest, entries, err := parseBackupArchiveFileToPaths(plainPath, entriesDir)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ensureRestoreDiskSpace(a.dbPath, entries); err != nil {
		a.jsonErr(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	if jwtSecretEphemeral {
		hasToken, tokenErr := backupHasJWTProtectedTokenFile(entries[backupDatabaseEntry])
		if tokenErr != nil {
			a.jsonErr(w, http.StatusBadRequest, "恢复校验失败：无法检查加密 Token 配置")
			return
		}
		if hasToken {
			a.jsonErr(w, http.StatusConflict, "当前 JWT_SECRET 不是持久密钥，无法安全恢复 Telegram 或 TMDB Token；请先配置稳定密钥")
			return
		}
	}
	targetHeaderKey := []byte(nil)
	if a.pm != nil {
		targetHeaderKey = a.pm.upstreamHeaderKey
	}
	if len(targetHeaderKey) != 32 {
		oldHeaderKey, keyErr := base64.RawStdEncoding.DecodeString(manifest.UpstreamHeaderKey)
		if keyErr != nil {
			a.jsonErr(w, http.StatusBadRequest, "恢复校验失败：备份中的上游请求头密钥无效")
			return
		}
		if len(oldHeaderKey) == 32 {
			tempDir, tempErr := os.MkdirTemp("", ".meridian-header-check-*")
			if tempErr != nil {
				a.jsonErr(w, http.StatusInternalServerError, "恢复校验失败")
				return
			}
			tempDB := filepath.Join(tempDir, backupDatabaseEntry)
			writeErr := copyPrivateFile(entries[backupDatabaseEntry], tempDB)
			hasHeaders := false
			if writeErr == nil {
				checkDB, openErr := sql.Open("sqlite", "file:"+filepath.ToSlash(tempDB)+"?mode=ro&_pragma=query_only(1)")
				if openErr == nil {
					var count int
					writeErr = checkDB.QueryRow("SELECT COUNT(*) FROM sites WHERE upstream_headers <> '' AND upstream_headers <> '[]'").Scan(&count)
					hasHeaders = count > 0
					_ = checkDB.Close()
				} else {
					writeErr = openErr
				}
			}
			_ = os.RemoveAll(tempDir)
			if writeErr != nil {
				a.jsonErr(w, http.StatusBadRequest, "恢复校验失败：无法检查自定义上游请求头")
				return
			}
			if hasHeaders {
				a.jsonErr(w, http.StatusConflict, "当前 UPSTREAM_HEADER_KEY 未配置，无法安全恢复自定义上游请求头")
				return
			}
		}
	}
	var preservedPanelSettings *backupPanelSettings
	if !manifestIncludesTLS(manifest) {
		preservedPanelSettings, err = readBackupPanelSettings(a.db.db)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "恢复校验失败：无法读取当前 TLS 设置")
			return
		}
	}
	targetHostIngressWithoutTLS := a.panelBindLoopback || len(a.trustedProxies) > 0
	resetIngressCount, err := writeRestorePendingFromPaths(a.dbPath, manifest, entries, jwtSecret, targetHeaderKey, preservedPanelSettings, targetHostIngressWithoutTLS)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, "恢复校验失败："+err.Error())
		return
	}
	a.clearSessionCookie(w, r)
	message := "备份已通过校验，Meridian 正在重启并应用恢复数据"
	if resetIngressCount > 0 {
		message += fmt.Sprintf("；%d 个站点的原入口不适用于当前服务器，已保留站点、清空入口并停用，请恢复后编辑入口再启用", resetIngressCount)
	}
	a.jsonOK(w, map[string]interface{}{
		"restarting":          true,
		"message":             message,
		"ingress_reset_count": resetIngressCount,
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if a.restartCh != nil {
		a.restartOnce.Do(func() {
			time.AfterFunc(500*time.Millisecond, func() { close(a.restartCh) })
		})
	}
}
