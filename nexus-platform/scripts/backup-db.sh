#!/usr/bin/env bash
# backup-db.sh — dump toàn bộ MySQL của Nexus + tar thư mục file đính kèm, xoay vòng theo ngày.
#
# VÌ SAO TỒN TẠI: mô hình doanh thu (VISION-AND-DELIVERY.md §6b chân 2) là Nexus **host mọi tenant
# khách trên hạ tầng của mình**. Trước script này, repo không có bất kỳ đường backup nào — một
# migration hỏng hoặc một thao tác nhầm của operator là mất dữ liệu NGHIỆP VỤ THẬT của khách,
# không đường về. Hosting mà không có backup không phải là một chân doanh thu, nó là một khoản nợ.
#
# MỘT CẶP, KHÔNG PHẢI MỘT FILE: `sys_files` chỉ là danh mục — byte thật nằm ở FILE_STORAGE_DIR.
# Dump DB mà không tar thư mục file thì "khôi phục" trả về danh mục trỏ vào byte chưa bao giờ
# được sao lưu (khách mất file vĩnh viễn dù có backup). Thứ tự BẮT BUỘC: dump DB TRƯỚC, tar file
# SAU — file tải lên xen giữa hai bước chỉ thành blob mồ côi vô hại; làm ngược lại thì thành
# dòng-không-có-file và bài diễn tập khôi phục (deploy/nexus/scripts/restore-drill.sh) đỏ giả.
#
# PHẠM VI (chọn có chủ đích): dump TOÀN CỤM, không per-tenant — đây là LƯỚI THẢM HOẠ. Sao lưu
# riêng từng khách (một tệp zip của MỘT khách, tải về / khôi phục riêng) là tính năng khác, nằm
# trong app: specs/068-tenant-backup-restore (TENANT_BACKUP_DIR). Bản đó nằm cùng máy với MySQL nên
# KHÔNG thay được script này.
#
# Dùng:
#   ./scripts/backup-db.sh                 # cặp vào ./backups/ (mặc định), giữ 14 cặp
#   BACKUP_DIR=/mnt/bk KEEP=30 ./scripts/backup-db.sh
#   crontab: 0 2 * * * cd /path/to/nexus-platform && ./scripts/backup-db.sh >> /var/log/nexus-backup.log 2>&1
#
# Restore (đọc kỹ — thao tác phá huỷ, lấy lại TOÀN BỘ cụm về thời điểm dump; làm CẢ HAI nửa):
#   gunzip < backups/nexus-YYYYMMDD-HHMMSS.sql.gz | mysql -h<host> -u<user> -p <db>
#   tar xzf backups/nexus-YYYYMMDD-HHMMSS.files.tar.gz -C "$FILE_STORAGE_DIR"   # giải nén ĐÈ, không xoá
# Kiểm cặp có khôi phục được không (mọi dòng sys_files sống có file đúng cỡ byte):
#   ../deploy/nexus/scripts/restore-drill.sh --verify <container-mysql> <db> "$FILE_STORAGE_DIR"
set -euo pipefail

cd "$(dirname "$0")/.."

# Đọc DATABASE_URL + FILE_STORAGE_DIR từ .env — đúng nguồn server dùng, không thêm nguồn cấu hình
# thứ hai. DATABASE_URL dạng: "user:pass@tcp(host:port)/dbname?..."
DBURL=$(grep -E '^DATABASE_URL=' .env | head -1 | cut -d= -f2- | tr -d '"')
if [ -z "$DBURL" ]; then
  echo "LỖI: không đọc được DATABASE_URL từ .env" >&2
  exit 1
fi
DB_USER=$(printf '%s' "$DBURL" | sed -E 's|^([^:]*):.*|\1|')
DB_PASS=$(printf '%s' "$DBURL" | sed -E 's|^[^:]*:([^@]*)@.*|\1|')
DB_HOST=$(printf '%s' "$DBURL" | sed -E 's|.*@tcp\(([^:]*):.*|\1|')
DB_PORT=$(printf '%s' "$DBURL" | sed -E 's|.*@tcp\([^:]*:([0-9]+)\).*|\1|')
DB_NAME=$(printf '%s' "$DBURL" | sed -E 's|.*/([^?]+).*|\1|')
# Cùng default với internal/config/file_config.go (defaultFileStorageDir).
STORAGE_DIR=$(grep -E '^FILE_STORAGE_DIR=' .env | head -1 | cut -d= -f2- | tr -d '"')
STORAGE_DIR=${STORAGE_DIR:-./storage}

BACKUP_DIR=${BACKUP_DIR:-./backups}
KEEP=${KEEP:-14}
STAMP=$(date +%Y%m%d-%H%M%S)
OUT="$BACKUP_DIR/nexus-$STAMP.sql.gz"
OUT_FILES="$BACKUP_DIR/nexus-$STAMP.files.tar.gz"

mkdir -p "$BACKUP_DIR"

# Nếu chết giữa chừng (mysqldump hỏng, pipefail giết script trước bước kiểm), file dump dở dang
# vẫn nằm lại — lần chạy đầu của chính script này đã để lại một file 20 byte như thế. Một file
# nhỏ-mà-hợp-lệ-gzip là backup GIẢ nguy hiểm nhất: nó qua được gzip -t và chiếm một suất xoay vòng.
# Cặp nửa vời cũng vậy — trap xoá cả hai, kể cả khi bị giết giữa chừng (Ctrl-C, rớt SSH, timeout).
trap 'rm -f "$OUT" "$OUT_FILES"' ERR
trap 'rm -f "$OUT" "$OUT_FILES"; exit 1' INT TERM HUP

# --single-transaction: dump nhất quán trên InnoDB mà KHÔNG khoá bảng — server cứ chạy tiếp.
# --routines --triggers: các down-migration dùng stored procedure; thiếu là restore ra schema khuyết.
# --hex-blob: cột JSON/binary sống sót qua round-trip mọi charset.
# MySQL của dự án chạy trong Docker (docker-compose) — máy host thường KHÔNG có mysqldump.
# Ưu tiên binary trên host nếu có; không thì exec vào container, đúng cách mọi truy cập DB
# khác của dự án này vẫn làm. Cả hai nhánh cùng cờ, cùng kiểm tra — chỉ khác đường vào.
DUMP_FLAGS="--single-transaction --routines --triggers --hex-blob"
# $DUMP_FLAGS cố ý không ngoặc kép: một chuỗi nhiều cờ, phải tách từ (SC2086 là chủ đích).
# shellcheck disable=SC2086
if command -v mysqldump >/dev/null 2>&1; then
  if [ -n "$DB_PASS" ]; then
    mysqldump -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" -p"$DB_PASS" $DUMP_FLAGS "$DB_NAME" | gzip > "$OUT"
  else
    mysqldump -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" $DUMP_FLAGS "$DB_NAME" | gzip > "$OUT"
  fi
else
  # Container nào publish đúng cổng trong DATABASE_URL là container đúng (máy dev có thể chạy
  # nhiều MySQL — grep tên "mysql" từng vớ nhầm container khác, sai mật khẩu). Không thấy thì
  # mới rơi về tên.
  MYSQL_CONTAINER=$(docker ps --filter "publish=$DB_PORT" --format '{{.Names}}' | head -1)
  [ -n "$MYSQL_CONTAINER" ] || MYSQL_CONTAINER=$(docker ps --format '{{.Names}}' | grep -i mysql | head -1)
  if [ -z "$MYSQL_CONTAINER" ]; then
    echo "LỖI: không có mysqldump trên host và không thấy container MySQL nào đang chạy." >&2
    exit 1
  fi
  # Trong container thì host là localhost; macOS bash 3.2 + set -u nên nhánh if, không mảng rỗng.
  if [ -n "$DB_PASS" ]; then
    docker exec "$MYSQL_CONTAINER" mysqldump -u"$DB_USER" -p"$DB_PASS" $DUMP_FLAGS "$DB_NAME" | gzip > "$OUT"
  else
    docker exec "$MYSQL_CONTAINER" mysqldump -u"$DB_USER" $DUMP_FLAGS "$DB_NAME" | gzip > "$OUT"
  fi
fi

# Một file dump rỗng/cụt là backup GIẢ — tệ hơn không có, vì nó cho cảm giác an toàn.
# gzip -t bắt file cụt; cỡ tối thiểu bắt dump rỗng (schema không thôi đã ~vài chục KB).
gzip -t "$OUT"
SIZE=$(wc -c < "$OUT" | tr -d ' ')
if [ "$SIZE" -lt 10240 ]; then
  echo "LỖI: dump chỉ ${SIZE} byte — gần như chắc chắn hỏng. GIỮ NGUYÊN bản cũ, không xoay vòng." >&2
  mv "$OUT" "$OUT.corrupt"
  exit 1
fi

# Nửa thứ hai — file đính kèm, SAU dump DB (xem đầu file vì sao thứ tự này bắt buộc). Thư mục
# chưa có = chưa ai tải file lên; vẫn ghi tar rỗng để MỌI snapshot đều là một cặp.
if [ -d "$STORAGE_DIR" ]; then
  tar czf "$OUT_FILES" -C "$STORAGE_DIR" .
else
  tar czf "$OUT_FILES" -T /dev/null
fi
gzip -t "$OUT_FILES"
FILES_N=$(tar tzf "$OUT_FILES" | grep -vc '/$' || true)

# Xoay vòng CHỈ sau khi cặp mới đã được kiểm — thứ tự này là chủ đích: không bao giờ
# xoá bản cũ để đổi lấy một bản mới chưa biết lành hay hỏng. Xoá theo CẶP.
# Tên file do script kiểm soát (không khoảng trắng), cần sắp theo thời gian → ls -t là đúng việc.
# shellcheck disable=SC2012
ls -1t "$BACKUP_DIR"/nexus-*.sql.gz 2>/dev/null | tail -n +$((KEEP + 1)) | while read -r old; do
  rm -f "$old" "${old%.sql.gz}.files.tar.gz"
done

echo "OK: $OUT ($(numfmt --to=iec "$SIZE" 2>/dev/null || echo "${SIZE}B")) + $OUT_FILES (${FILES_N} file từ $STORAGE_DIR) · giữ $KEEP cặp mới nhất"
