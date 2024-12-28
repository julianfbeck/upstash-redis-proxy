const { Redis } = require('@upstash/redis')

// Create Redis client pointing to your local proxy
const redis = new Redis({
  url: 'http://localhost:8080',
  token: 'default-token',
})

async function runTests() {
  try {
    // Test SET and GET
    console.log('\nTesting SET and GET:')
    await redis.set('test-key', 'Hello from Node.js!')
    const value = await redis.get('test-key')
    console.log('GET result:', value)

    // Test INCR
    console.log('\nTesting INCR:')
    await redis.set('counter', '0')
    const newCount = await redis.incr('counter')
    console.log('INCR result:', newCount)

    // Test EXISTS
    console.log('\nTesting EXISTS:')
    const exists = await redis.exists('test-key')
    console.log('EXISTS result:', exists)

    // Test DEL
    console.log('\nTesting DEL:')
    await redis.del('test-key')
    const afterDel = await redis.exists('test-key')
    console.log('After DEL exists?:', afterDel)

    // Test EXPIRE and TTL
    console.log('\nTesting EXPIRE and TTL:')
    await redis.set('expire-key', 'will expire')
    await redis.expire('expire-key', 10)
    const ttl = await redis.ttl('expire-key')
    console.log('TTL result:', ttl)

    // Test Pipeline
    console.log('\nTesting Pipeline:')
    const pipeline = redis.pipeline()
    pipeline.set('pipeline-key', 'value1')
    pipeline.set('pipeline-key2', 'value2')
    pipeline.get('pipeline-key')
    pipeline.get('pipeline-key2')
    pipeline.del('pipeline-key', 'pipeline-key2')
    const results = await pipeline.exec()
    console.log('Pipeline results:', results)

    // Clean up
    await redis.del('counter', 'expire-key')
    console.log('\nTest completed successfully!')

  } catch (error) {
    console.error('Error during tests:', error)
  }
}

runTests()
