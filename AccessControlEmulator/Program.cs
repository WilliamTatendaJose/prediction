using System;
using System.Net.Http;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace AccessControlSimulator
{
    class Program
    {
        private static readonly HttpClient _httpClient = new HttpClient();
        private static readonly string _apiUrl = "http://localhost:5270/api/predict"; // Replace with your API URL
        private static readonly string _apiUrl1 = "http://localhost:5270/api/inputdata"; // Replace with your API URL


        static async Task Main(string[] args)
        {
            Console.WriteLine("Access Control Simulator Started...");

            while (true)
            {
                // Generate random access control data
                var accessControlData = GenerateRandomAccessControlData();

                // Convert data to JSON
                var json = JsonSerializer.Serialize(accessControlData);
                var content = new StringContent(json, Encoding.UTF8, "application/json");

                // Send data to the API
                try
                {
                    var response = await _httpClient.PostAsync(_apiUrl, content);
                    var response1 = await _httpClient.PostAsync(_apiUrl1, content);
                    if (response.IsSuccessStatusCode)
                    {
                        Console.WriteLine($"Data sent successfully: {json}");
                    }
                    else
                    {
                        Console.WriteLine($"Failed to send data. Status Code: {response}");
                    }
                }
                catch (Exception ex)
                {
                    Console.WriteLine($"Error sending data: {ex.Message}");
                }

                // Wait for 30 seconds before sending the next data
                Thread.Sleep(30000);
            }
        }

        private static AccessControlData GenerateRandomAccessControlData()
        {
            var random = new Random();
            var locations = new[] { "Main Entrance", "Back Door", "Server Room", "Parking Lot" };
            var accessMethods = new[] { "Keycard", "Biometric", "PIN Code" };
            var accessStatuses = new[] { "Success", "Failed" };
            var daysOfWeek = new[] { "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday" };
            var doorSensorStatuses = new[] { "Open", "Closed" };
            var isHoliday = new[] { "True", "False" };

            return new AccessControlData
            {
                //SystemId = $"ACS-{random.Next(10000, 99999)}",
                Timestamp = DateTime.UtcNow.ToString("o"),
                Class = "Normal",
                UserID = "User123",
                Input = " normal",
                LocationID = locations[random.Next(locations.Length)],
                AccessMethod = accessMethods[random.Next(accessMethods.Length)],
                AccessStatus = accessStatuses[random.Next(accessStatuses.Length)],
                DayOfWeek = daysOfWeek[random.Next(daysOfWeek.Length)],
                HourOfDay = random.Next(0, 24),
                IsHoliday = isHoliday[random.Next(2)], // Randomly true or false
                DoorSensorStatus = doorSensorStatuses[random.Next(doorSensorStatuses.Length)],
                VibrationSensorReading = (float)random.NextDouble() * 2, // Random value between 0 and 10
                Temperature = (float)random.Next(15, 35), // Random temperature between 15 and 35
                Humidity = (float)random.Next(30, 70) // Random humidity between 30 and 70
                                                     



            };

        }
    }

    public class AccessControlData
    {
        public string SystemId { get; set; }
        public string Timestamp { get; set; }
        public string Class { get; set; }
        public string Input { get; set; }
        public string LocationID { get; set; }
        public string AccessMethod { get; set; }
        public string AccessStatus { get; set; }
        public string DayOfWeek { get; set; }
        public float HourOfDay { get; set; }
        public string IsHoliday { get; set; }
        public string DoorSensorStatus { get; set; }
        public float VibrationSensorReading { get; set; }
        public float Temperature { get; set; }
        public float Humidity { get; set; }
        public string UserID { get; internal set; }
    }
}