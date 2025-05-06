#!/bin/bash
# test_version.sh - Tests Makefile version extraction for a provided Git tag format

# Set up colors for better output
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Check if a version was provided
if [ $# -eq 0 ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v1.2.3"
    echo "Example: $0 v1.2.3-5-g12345"
    exit 1
fi

# Get the version from command line
gitversion="$1"

# Create a temporary file to hold our test Makefile
TEMP_MAKEFILE=$(mktemp)

# Copy the version extraction part of your Makefile to the temp file
cat > $TEMP_MAKEFILE << 'EOF'
# Version regex pattern
VERSION_REGEX := v([0-9]+)\.([0-9]+)\.([0-9]+)

# Function to test different GITVERSION values
test-version:
	@echo "Testing version: $(GITVERSION)"
	@echo "Major: $(MAJOR)"
	@echo "Minor: $(MINOR)"
	@echo "Patch: $(PATCH)"
	@echo ""

# Parse version components from GITVERSION
ifeq ($(shell echo $(GITVERSION) | grep -E "^$(VERSION_REGEX)$$"),$(GITVERSION))
  # Clean version tag like v1.2.3
  MAJOR := $(shell echo $(GITVERSION) | sed -E "s/^$(VERSION_REGEX).*/\1/")
  MINOR := $(shell echo $(GITVERSION) | sed -E "s/^$(VERSION_REGEX).*/\2/")
  PATCH := $(shell echo $(GITVERSION) | sed -E "s/^$(VERSION_REGEX).*/\3/")
else ifeq ($(shell echo $(GITVERSION) | grep -E "^$(VERSION_REGEX)-"),$(shell echo $(GITVERSION) | grep -E "^$(VERSION_REGEX)-"))
  # Version tag with additional info like v1.2.3-5-g12345
  MAJOR := $(shell echo $(GITVERSION) | sed -E "s/^$(VERSION_REGEX).*/\1/")
  MINOR := $(shell echo $(GITVERSION) | sed -E "s/^$(VERSION_REGEX).*/\2/")
  PATCH := $(shell echo $(GITVERSION) | sed -E "s/^$(VERSION_REGEX).*/\3/")
else
  # Default if no pattern match
  MAJOR := 0
  MINOR := 0
  PATCH := 1
endif
EOF

# Function to run a test case
run_test() {
    local gitversion=$1
    echo -e "${BLUE}Testing version extraction for: $gitversion${NC}"
    
    # Run make with this GITVERSION value and capture the output
    result=$(make -f $TEMP_MAKEFILE test-version GITVERSION="$gitversion")
    echo "$result"
    
    # Extract the values
    major=$(echo "$result" | grep "Major:" | cut -d' ' -f2)
    minor=$(echo "$result" | grep "Minor:" | cut -d' ' -f2)
    patch=$(echo "$result" | grep "Patch:" | cut -d' ' -f2)
    
    # Determine expected values
    if [[ $gitversion =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
        expected_major=${BASH_REMATCH[1]}
        expected_minor=${BASH_REMATCH[2]}
        expected_patch=${BASH_REMATCH[3]}
        
        # Check if values match expectations
        if [[ "$major" == "$expected_major" && "$minor" == "$expected_minor" && "$patch" == "$expected_patch" ]]; then
            echo -e "${GREEN}✓ Test passed${NC}"
        else
            echo -e "${RED}✗ Test failed${NC}"
            echo "  Expected: $expected_major.$expected_minor.$expected_patch"
            echo "  Got: $major.$minor.$patch"
        fi
    else
        # For invalid formats, we expect defaults
        expected_major=0
        expected_minor=0
        expected_patch=1
        
        if [[ "$major" == "$expected_major" && "$minor" == "$expected_minor" && "$patch" == "$expected_patch" ]]; then
            echo -e "${GREEN}✓ Test passed (defaults applied)${NC}"
        else
            echo -e "${RED}✗ Test failed (defaults not applied correctly)${NC}"
            echo "  Expected: $expected_major.$expected_minor.$expected_patch"
            echo "  Got: $major.$minor.$patch"
        fi
    fi
}

# Run the test with the provided version
run_test "$gitversion"

# Clean up
rm $TEMP_MAKEFILE