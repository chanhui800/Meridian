package main

import (
	"database/sql"
	"sort"
	"strings"
)

// siteDNSRecord is one published address record for a site. A dual-stack node
// needs two of these (one A and one AAAA) for the same hostname, which is why
// they live in their own table instead of the single cf_record_id column on
// site_node_schedules.
type siteDNSRecord struct {
	Family     string
	ZoneID     string
	RecordID   string
	RecordType string
	Address    string
	// previous is the in-memory compensation preimage for the publish attempt
	// that produced this record. It is never persisted.
	previous *cloudflareAddressRecord
}

// dnsFamilyOrder keeps the published families in a stable order so a comparison
// or a stored value never depends on map iteration.
func dnsFamilyOrder() []string { return []string{"v4", "v6"} }

func normalizeDNSFamilies(families []string) []string {
	seen := make(map[string]bool, len(families))
	result := make([]string, 0, len(families))
	for _, family := range dnsFamilyOrder() {
		for _, candidate := range families {
			if candidate != family || seen[family] {
				continue
			}
			seen[family] = true
			result = append(result, family)
		}
	}
	return result
}

func encodeDNSFamilies(families []string) string {
	return strings.Join(normalizeDNSFamilies(families), ",")
}

func decodeDNSFamilies(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	families := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			families = append(families, trimmed)
		}
	}
	if len(families) == 0 {
		return nil
	}
	return normalizeDNSFamilies(families)
}

func dnsFamiliesEqual(left, right []string) bool {
	normalizedLeft, normalizedRight := normalizeDNSFamilies(left), normalizeDNSFamilies(right)
	if len(normalizedLeft) != len(normalizedRight) {
		return false
	}
	for index := range normalizedLeft {
		if normalizedLeft[index] != normalizedRight[index] {
			return false
		}
	}
	return true
}

// siteDNSRecordSelect keeps the column list in one place so every reader stays
// aligned with the table.
const siteDNSRecordSelect = `SELECT family,zone_id,record_id,record_type,address FROM site_node_dns_records`

func scanSiteDNSRecords(rows *sql.Rows) ([]siteDNSRecord, error) {
	records := make([]siteDNSRecord, 0, 2)
	for rows.Next() {
		var record siteDNSRecord
		if err := rows.Scan(&record.Family, &record.ZoneID, &record.RecordID, &record.RecordType, &record.Address); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Family < records[j].Family })
	return records, nil
}

// siteDNSRecords returns the published records for one site in family order.
func (d *DB) siteDNSRecords(siteID int64) ([]siteDNSRecord, error) {
	rows, err := d.db.Query(siteDNSRecordSelect+" WHERE site_id=? ORDER BY family", siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSiteDNSRecords(rows)
}

// replaceSiteDNSRecordsTx makes the given set the site's complete published set.
// Records that are no longer present are dropped, so a family the scheduler
// deliberately stopped publishing cannot leave a tracked row behind.
func replaceSiteDNSRecordsTx(tx *sql.Tx, siteID int64, records []siteDNSRecord, nowMS int64) error {
	if tx == nil || siteID <= 0 {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM site_node_dns_records WHERE site_id=?", siteID); err != nil {
		return err
	}
	for _, record := range normalizeDNSRecords(records) {
		if _, err := tx.Exec(`INSERT INTO site_node_dns_records(site_id,family,zone_id,record_id,record_type,address,updated_at_ms)
			VALUES(?,?,?,?,?,?,?)`, siteID, record.Family, record.ZoneID, record.RecordID, record.RecordType, record.Address, nowMS); err != nil {
			return err
		}
	}
	return nil
}

// normalizeDNSRecords keeps only well-formed, uniquely-familied records in
// family order. A record without a remote ID or without a literal address of its
// own family is not something the scheduler could reconcile, so it is dropped
// rather than tracked as a phantom.
func normalizeDNSRecords(records []siteDNSRecord) []siteDNSRecord {
	byFamily := make(map[string]siteDNSRecord, len(records))
	for _, record := range records {
		family := strings.TrimSpace(record.Family)
		if family != "v4" && family != "v6" {
			continue
		}
		if strings.TrimSpace(record.RecordID) == "" {
			continue
		}
		record.Family = family
		record.ZoneID = strings.TrimSpace(record.ZoneID)
		record.RecordID = strings.TrimSpace(record.RecordID)
		record.RecordType = dnsRecordType(family)
		record.Address = strings.TrimSpace(record.Address)
		// The address must belong to the family it is filed under, otherwise the
		// next reconcile would write an A record carrying an IPv6 value.
		if nodeAddressFamily(record.Address) != family {
			continue
		}
		byFamily[family] = record
	}
	result := make([]siteDNSRecord, 0, len(byFamily))
	for _, family := range dnsFamilyOrder() {
		if record, ok := byFamily[family]; ok {
			result = append(result, record)
		}
	}
	return result
}

// deleteSiteDNSRecordsTx clears the tracked set. Used when a site's DNS is
// removed or its schedule is disabled.
func deleteSiteDNSRecordsTx(tx *sql.Tx, siteID int64) error {
	if tx == nil || siteID <= 0 {
		return nil
	}
	_, err := tx.Exec("DELETE FROM site_node_dns_records WHERE site_id=?", siteID)
	return err
}

func (d *DB) deleteSiteDNSRecords(siteID int64) error {
	if d == nil || siteID <= 0 {
		return nil
	}
	_, err := d.db.Exec("DELETE FROM site_node_dns_records WHERE site_id=?", siteID)
	return err
}
