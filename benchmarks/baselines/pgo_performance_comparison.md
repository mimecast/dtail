# PGO (Profile-Guided Optimization) Performance Comparison

## Summary

This analysis compares the performance metrics between pre-PGO baseline (baseline_20250704_133210_pre-pgo-baseline.txt) and post-PGO optimized (baseline_20250704_133941_post-pgo-optimized.txt) benchmarks.

## Performance Improvements by Operation

### DCat Operations

| Test Case | Pre-PGO (ns/op) | Post-PGO (ns/op) | Improvement | Pre-PGO (MB/s) | Post-PGO (MB/s) | Throughput Gain |
|-----------|-----------------|------------------|-------------|----------------|-----------------|-----------------|
| Size=10MB | 16,848,805 | 16,216,111 | **3.75%** | 255.1 | 259.9 | **1.88%** |
| Size=100MB | 125,358,735 | 120,403,497 | **3.95%** | 325.5 | 339.2 | **4.21%** |
| Size=1GB | 1,358,405,900 | 1,285,097,913 | **5.40%** | 311.8 | 330.4 | **5.96%** |

### DGrep Operations

| Test Case | Pre-PGO (ns/op) | Post-PGO (ns/op) | Improvement | Pre-PGO (MB/s) | Post-PGO (MB/s) | Throughput Gain |
|-----------|-----------------|------------------|-------------|----------------|-----------------|-----------------|
| **10MB Tests** |
| HitRate=1% | 10,631,785 | 9,579,392 | **9.90%** | 388.0 | 417.9 | **7.71%** |
| HitRate=10% | 12,514,942 | 12,894,868 | -3.04% | 328.2 | 324.0 | -1.28% |
| HitRate=50% | 15,555,715 | 14,874,639 | **4.38%** | 273.1 | 285.3 | **4.46%** |
| HitRate=90% | 18,455,157 | 16,490,247 | **10.65%** | 239.7 | 265.6 | **10.81%** |
| **100MB Tests** |
| HitRate=1% | 86,373,951 | 81,839,519 | **5.25%** | 464.7 | 493.5 | **6.20%** |
| HitRate=10% | 94,793,919 | 109,455,727 | -15.47% | 433.9 | 387.7 | -10.64% |
| HitRate=50% | 125,103,249 | 150,064,433 | -19.95% | 346.8 | 289.6 | -16.48% |
| HitRate=90% | 143,482,368 | 155,150,309 | -8.13% | 310.5 | 288.6 | -7.06% |
| **1GB Tests** |
| HitRate=1% | 1,020,215,198 | 825,743,710 | **19.06%** | 426.7 | 507.8 | **19.01%** |
| HitRate=10% | 987,330,253 | 1,123,188,972 | -13.76% | 436.6 | 394.1 | -9.74% |
| HitRate=50% | 1,238,384,740 | 2,163,640,075 | -74.73% | 366.4 | 216.3 | -40.96% |
| HitRate=90% | 1,701,114,334 | 2,908,900,743 | -71.00% | 287.6 | 166.5 | -42.11% |

### DMap Operations

| Test Case | Pre-PGO (ns/op) | Post-PGO (ns/op) | Improvement | Pre-PGO (MB/s) | Post-PGO (MB/s) | Throughput Gain |
|-----------|-----------------|------------------|-------------|----------------|-----------------|-----------------|
| **10MB Tests** |
| Query=count | 357,896,674 | 502,789,906 | -40.48% | 21.72 | 15.45 | -28.87% |
| Query=sum_avg | 361,951,190 | 455,288,778 | -25.78% | 21.44 | 17.05 | -20.47% |
| Query=min_max | 363,040,718 | 367,933,848 | -1.35% | 21.36 | 21.08 | -1.31% |
| Query=multi | 371,280,543 | 363,108,940 | **2.20%** | 20.90 | 21.34 | **2.11%** |
| **100MB Tests** |
| Query=count | 1,643,333,704 | 1,850,882,955 | -12.63% | 47.53 | 42.05 | -11.53% |
| Query=sum_avg | 1,890,566,330 | 2,054,243,726 | -8.66% | 41.09 | 37.85 | -7.89% |
| Query=min_max | 1,854,683,475 | 1,935,445,223 | -4.35% | 41.80 | 40.24 | -3.73% |
| Query=multi | 1,943,425,833 | 2,281,991,922 | -17.42% | 39.99 | 34.07 | -14.80% |
| **1GB Tests** |
| Query=count | 16,707,468,357 | 18,175,390,172 | -8.78% | 47.42 | 43.60 | -8.06% |
| Query=sum_avg | 17,837,207,478 | 17,415,924,780 | **2.36%** | 44.47 | 45.55 | **2.43%** |
| Query=min_max | 27,596,912,470 | 16,822,541,213 | **39.03%** | 28.67 | 47.21 | **64.70%** |
| Query=multi | 18,380,794,254 | 17,971,202,125 | **2.23%** | 43.08 | 44.06 | **2.27%** |

## Key Findings

### Positive Impacts of PGO:

1. **DCat Operations**: Consistent improvements across all sizes
   - 3.75% to 5.40% reduction in execution time
   - Up to 5.96% throughput improvement for 1GB files

2. **DGrep with Low Hit Rates**: Significant improvements
   - Up to 19.06% improvement for 1GB files with 1% hit rate
   - Best improvements seen with lower hit rates (1%)

3. **DMap min_max Query on 1GB**: Exceptional improvement
   - 39.03% reduction in execution time
   - 64.70% throughput improvement

### Mixed or Negative Impacts:

1. **DGrep with High Hit Rates**: Performance degradation
   - Larger files with high hit rates (50%, 90%) show significant slowdowns
   - Up to 74.73% slower for 1GB files with 50% hit rate

2. **DMap count and sum_avg Queries**: Generally slower
   - Most DMap operations show regression except for min_max and multi queries
   - Count queries particularly affected (-40.48% for 10MB)

## Conclusion

PGO optimization shows:
- **Consistent benefits** for DCat operations (file reading)
- **Mixed results** for DGrep depending on hit rate (better for low hit rates, worse for high)
- **Variable impact** on DMap queries (excellent for min_max on large files, regression for count/sum_avg)

The optimization appears to be most effective for:
1. Sequential read operations (DCat)
2. Search operations with sparse matches (DGrep with low hit rates)
3. Specific MapReduce queries (min_max on large datasets)

Areas where PGO may need tuning:
1. High-match-rate grep operations
2. Count and aggregation MapReduce queries