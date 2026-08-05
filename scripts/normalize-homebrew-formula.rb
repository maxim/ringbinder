#!/usr/bin/env ruby
# frozen_string_literal: true

path, version, *extra = ARGV
abort "usage: normalize-homebrew-formula.rb PATH VERSION" unless path && version && extra.empty?

formula = File.read(path)
version_line = %(  version "#{version}"\n)
count = formula.lines.count { |line| line == version_line }
abort "expected exactly one redundant formula version, found #{count}" unless count == 1

normalized = formula.sub(version_line, "")
abort "an explicit formula version remains" if normalized.match?(/^\s*version\b/)

File.write(path, normalized)
