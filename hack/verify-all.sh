#!/usr/bin/env bash

# Copyright 2025 The KubeLB Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

cd "$(dirname "$0")/.."

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Track failures
declare -a FAILED_CHECKS=()
declare -a PASSED_CHECKS=()

# Function to run a check and track result
run_check() {
  local check_name="$1"
  local check_command="$2"

  echo ""
  echo "=========================================="
  echo "Running: ${check_name}"
  echo "=========================================="

  if eval "${check_command}"; then
    echo -e "${GREEN}✓ ${check_name} passed${NC}"
    PASSED_CHECKS+=("${check_name}")
  else
    echo -e "${RED}✗ ${check_name} failed${NC}"
    FAILED_CHECKS+=("${check_name}")
  fi
}

# Run all verification checks
echo "Starting all verification checks..."

# 1. Check dependencies
run_check "Dependency verification" "go mod verify"

# 2. YAML lint
run_check "YAML lint" "yamllint -c .yamllint.conf ."

# 3. Boilerplate verification
run_check "Boilerplate verification" "./hack/verify-boilerplate.sh"

# 4. Import order verification
run_check "Import order verification" "./hack/verify-import-order.sh"

# 5. Shell script formatting
run_check "Shell script formatting" "shfmt -l -sr -i 2 -d hack"

# 6. License validation
run_check "License validation" "./hack/verify-licenses.sh"

# Print summary
echo ""
echo "=========================================="
echo "VERIFICATION SUMMARY"
echo "=========================================="

if [ ${#PASSED_CHECKS[@]} -gt 0 ]; then
  echo -e "${GREEN}Passed checks (${#PASSED_CHECKS[@]}):${NC}"
  for check in "${PASSED_CHECKS[@]}"; do
    echo -e "  ${GREEN}✓${NC} ${check}"
  done
fi

if [ ${#FAILED_CHECKS[@]} -gt 0 ]; then
  echo ""
  echo -e "${RED}Failed checks (${#FAILED_CHECKS[@]}):${NC}"
  for check in "${FAILED_CHECKS[@]}"; do
    echo -e "  ${RED}✗${NC} ${check}"
  done
fi

echo ""
echo "=========================================="

# Exit with failure if any check failed
if [ ${#FAILED_CHECKS[@]} -gt 0 ]; then
  echo -e "${RED}VERIFICATION FAILED: ${#FAILED_CHECKS[@]} check(s) failed${NC}"
  exit 1
else
  echo -e "${GREEN}ALL VERIFICATION CHECKS PASSED${NC}"
  exit 0
fi
